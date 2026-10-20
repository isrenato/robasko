// Package queue hands work between agents.
//
// SPEC.md §8: scout queues a step for analyst, analyst queues one for
// writer, and no agent reaches into another's work. The handover is a row
// in task_steps rather than a call, so a crash between the two leaves the
// step claimable by whoever comes back rather than losing it.
//
// The claim is the whole design. Three processes poll the same table, and
// FOR UPDATE SKIP LOCKED is what makes exactly one of them win a row
// without any of them waiting on the others.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Retry policy from SPEC.md §8.
const (
	MaxAttempts = 3
	BackoffBase = 5 * time.Second
)

// Step states.
const (
	Pending = "pending"
	Claimed = "claimed"
	Done    = "done"
	Failed  = "failed"
	Dead    = "dead"
)

// ErrNoWork reports an empty queue. It is not a failure: an idle agent is
// the normal state between cycles.
var ErrNoWork = errors.New("no claimable step")

// Step is one unit of handed-over work.
type Step struct {
	ID       string
	TaskID   string
	TraceID  string
	Seq      int
	ToolName string
	Version  int
	Input    json.RawMessage
	Attempts int
}

// Envelope is what the handover looks like in the log (SPEC.md §8). It
// exists so a trace shows who passed what to whom, not just that a row
// changed state.
type Envelope struct {
	TraceID  string    `json:"trace_id"`
	TaskID   string    `json:"task_id"`
	StepID   string    `json:"step_id"`
	Seq      int       `json:"seq"`
	Tool     string    `json:"tool"`
	From     string    `json:"from"`
	To       string    `json:"to"`
	Attempt  int       `json:"attempt"`
	Deadline time.Time `json:"deadline"`
}

// Queue is the handover. The interface exists because SPEC.md §3 wants a
// second implementation on SQS behind a flag; nothing above this package
// should know which one it is talking to.
type Queue interface {
	// Enqueue adds a step to a task.
	Enqueue(ctx context.Context, taskID, toolName string, input json.RawMessage) (*Step, error)
	// Claim takes the next claimable step for an agent, or ErrNoWork.
	//
	// tools names the step tools this agent handles. SPEC.md §8 gives
	// each role its own work - scout queues for analyst, analyst for
	// writer - and without the filter whichever process polled first
	// would take whatever was there. An empty list claims anything,
	// which is what a single-process run wants.
	Claim(ctx context.Context, agentID string, tools []string) (*Step, error)
	// Complete finishes a step with its result.
	Complete(ctx context.Context, stepID string, result json.RawMessage) error
	// Fail records a failure. The step returns to the queue with backoff
	// until it has been attempted MaxAttempts times, then it is dead and
	// its task is closed with the reason.
	Fail(ctx context.Context, stepID, errorCode string, cause error) (dead bool, err error)
	// Kill ends a step without further attempts, for a refusal that will
	// refuse again identically. Retrying a missing scope only burns
	// attempts and delays the task reaching a state someone can act on.
	Kill(ctx context.Context, stepID, errorCode string, cause error) error
}

// Postgres is the queue backed by task_steps.
type Postgres struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewPostgres returns a queue over an existing pool.
func NewPostgres(pool *pgxpool.Pool) *Postgres {
	return &Postgres{pool: pool, now: time.Now}
}

// CreateTask opens a task and returns its id. A task groups the steps
// that belong to one vacancy and carries the trace the whole chain shares.
func (q *Postgres) CreateTask(ctx context.Context, traceID, goal, subjectID, createdBy string) (string, error) {
	var id string
	err := q.pool.QueryRow(ctx, `
		INSERT INTO tasks (trace_id, goal, subject_id, created_by, status)
		VALUES ($1, $2, NULLIF($3, '')::uuid, NULLIF($4, '')::uuid, 'running')
		RETURNING id`, traceID, goal, subjectID, createdBy).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("queue: creating task: %w", err)
	}
	return id, nil
}

// Enqueue adds a step at the next free position in its task.
func (q *Postgres) Enqueue(ctx context.Context, taskID, toolName string, input json.RawMessage) (*Step, error) {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	var s Step
	err := q.pool.QueryRow(ctx, `
		INSERT INTO task_steps (task_id, seq, tool_name, input)
		VALUES ($1,
		        COALESCE((SELECT max(seq) + 1 FROM task_steps WHERE task_id = $1), 0),
		        $2, $3)
		RETURNING id, task_id, seq, tool_name, input, attempts`,
		taskID, toolName, input,
	).Scan(&s.ID, &s.TaskID, &s.Seq, &s.ToolName, &s.Input, &s.Attempts)
	if err != nil {
		return nil, fmt.Errorf("queue: enqueuing %s: %w", toolName, err)
	}
	return &s, nil
}

// Claim takes one step.
//
// The subquery locks a single row and skips any another agent already
// holds, so three agents polling together take three different steps
// rather than contending for one. Ordering by task and sequence keeps a
// task's steps in order without serialising unrelated tasks.
func (q *Postgres) Claim(ctx context.Context, agentID string, tools []string) (*Step, error) {
	var (
		s Step
		// Both are nullable: a step may pin no version, and a task may
		// predate the trace it would have carried.
		version *int
		trace   *string
	)
	err := q.pool.QueryRow(ctx, `
		UPDATE task_steps
		SET status = 'claimed', claimed_by = NULLIF($1, '')::uuid,
		    claimed_at = now(), attempts = attempts + 1
		WHERE id = (
			SELECT id FROM task_steps
			WHERE status = 'pending' AND available_at <= now()
			  AND (cardinality($2::text[]) = 0 OR tool_name = ANY($2::text[]))
			ORDER BY task_id, seq
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, task_id, seq, tool_name, tool_version, input, attempts,
		          (SELECT trace_id FROM tasks WHERE tasks.id = task_steps.task_id)`,
		agentID, nonNilTools(tools),
	).Scan(&s.ID, &s.TaskID, &s.Seq, &s.ToolName, &version, &s.Input, &s.Attempts, &trace)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoWork
	}
	if err != nil {
		return nil, fmt.Errorf("queue: claiming: %w", err)
	}
	if version != nil {
		s.Version = *version
	}
	if trace != nil {
		s.TraceID = *trace
	}
	return &s, nil
}

// Complete finishes a step, and closes its task when nothing is left.
func (q *Postgres) Complete(ctx context.Context, stepID string, result json.RawMessage) error {
	return q.inTx(ctx, func(tx pgx.Tx) error {
		var taskID string
		err := tx.QueryRow(ctx, `
			UPDATE task_steps SET status = 'done', result = $2
			WHERE id = $1 AND status = 'claimed'
			RETURNING task_id`, stepID, result).Scan(&taskID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("queue: step %s is not claimed", stepID)
		}
		if err != nil {
			return fmt.Errorf("queue: completing %s: %w", stepID, err)
		}
		return closeTaskIfSettled(ctx, tx, taskID)
	})
}

// Fail records a failure and decides what happens to the step.
//
// Backoff is 2^attempts × 5s (SPEC.md §8), so a step that fails for a
// transient reason comes back shortly and one that keeps failing stops
// occupying the queue. After MaxAttempts it is dead, and a dead step
// closes its task as failed - the daily report lists those rather than
// letting them sit unnoticed.
func (q *Postgres) Fail(ctx context.Context, stepID, errorCode string, cause error) (bool, error) {
	var dead bool
	err := q.inTx(ctx, func(tx pgx.Tx) error {
		var attempts int
		var taskID string
		if err := tx.QueryRow(ctx,
			`SELECT attempts, task_id FROM task_steps WHERE id = $1`, stepID,
		).Scan(&attempts, &taskID); err != nil {
			return fmt.Errorf("queue: reading step %s: %w", stepID, err)
		}

		if attempts >= MaxAttempts {
			dead = true
			if _, err := tx.Exec(ctx,
				`UPDATE task_steps SET status = 'dead', error_code = $2 WHERE id = $1`,
				stepID, errorCode); err != nil {
				return fmt.Errorf("queue: marking %s dead: %w", stepID, err)
			}
			reason := errorCode
			if cause != nil {
				reason = fmt.Sprintf("%s: %v", errorCode, cause)
			}
			_, err := tx.Exec(ctx, `
				UPDATE tasks SET status = 'failed', reason = $2, finished_at = now()
				WHERE id = $1 AND status NOT IN ('done', 'failed')`, taskID, reason)
			return err
		}

		backoff := time.Duration(1<<attempts) * BackoffBase
		_, err := tx.Exec(ctx, `
			UPDATE task_steps
			SET status = 'pending', error_code = $2, available_at = now() + $3::interval
			WHERE id = $1`, stepID, errorCode, fmt.Sprintf("%d seconds", int(backoff.Seconds())))
		if err != nil {
			return fmt.Errorf("queue: rescheduling %s: %w", stepID, err)
		}
		return nil
	})
	return dead, err
}

// Kill ends a step immediately, without the remaining attempts.
func (q *Postgres) Kill(ctx context.Context, stepID, errorCode string, cause error) error {
	return q.inTx(ctx, func(tx pgx.Tx) error {
		var taskID string
		err := tx.QueryRow(ctx, `
			UPDATE task_steps SET status = 'dead', error_code = $2
			WHERE id = $1
			RETURNING task_id`, stepID, errorCode).Scan(&taskID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("queue: no step %s", stepID)
		}
		if err != nil {
			return fmt.Errorf("queue: killing %s: %w", stepID, err)
		}

		reason := errorCode
		if cause != nil {
			reason = fmt.Sprintf("%s: %v", errorCode, cause)
		}
		_, err = tx.Exec(ctx, `
			UPDATE tasks SET status = 'failed', reason = $2, finished_at = now()
			WHERE id = $1 AND status NOT IN ('done', 'failed')`, taskID, reason)
		return err
	})
}

// closeTaskIfSettled marks a task done once no step can still run.
func closeTaskIfSettled(ctx context.Context, tx pgx.Tx, taskID string) error {
	var open int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM task_steps
		 WHERE task_id = $1 AND status IN ('pending', 'claimed')`, taskID,
	).Scan(&open); err != nil {
		return err
	}
	if open > 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		UPDATE tasks SET status = 'done', finished_at = now()
		WHERE id = $1 AND status = 'running'`, taskID)
	return err
}

func (q *Postgres) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Handover builds the envelope for a step passed from one role to another.
func Handover(s *Step, from, to string, deadline time.Time) Envelope {
	return Envelope{
		TraceID: s.TraceID, TaskID: s.TaskID, StepID: s.ID, Seq: s.Seq,
		Tool: s.ToolName, From: from, To: to, Attempt: s.Attempts, Deadline: deadline,
	}
}

func nonNilTools(tools []string) []string {
	if tools == nil {
		return []string{}
	}
	return tools
}

// Backoff is how long a step waits before its next attempt.
func Backoff(attempts int) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	return time.Duration(1<<attempts) * BackoffBase
}
