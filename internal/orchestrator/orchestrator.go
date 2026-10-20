// Package orchestrator runs one agent role against the queue.
//
// SPEC.md §8: three processes, one role each, each with its own token and
// scopes. scout finds vacancies and queues a scoring step; analyst scores
// and, when the result warrants it, queues a drafting step; writer drafts.
// No role performs another's writes, and the scope check is what makes
// that true rather than a convention.
//
// The worker itself is deliberately small. Claim a step, run its tool
// through the runtime, record the outcome. Everything that could go wrong
// - a missing scope, an argument that does not validate, a chain that
// runs too deep, a step retried after it already finished - is the
// runtime's and the queue's to refuse, and this loop only has to decide
// whether a refusal is worth retrying.
package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/isrenato/robasko/internal/queue"
	"github.com/isrenato/robasko/internal/runtime"
)

// Step tool names, which are also what a role claims.
const (
	StepScoreVacancy = "score_vacancy"
	StepDraftLetter  = "draft_letter"
)

// Role is one agent's job and what it may reach.
type Role struct {
	Name string
	// Scopes are checked by the runtime on every call. scout holds no
	// scores:write, so it cannot score even by calling the tool directly.
	Scopes []string
	// Handles are the step tools this role claims. A role claims only its
	// own work; see queue.Queue.Claim.
	Handles []string
}

// Roles are the three from SPEC.md §8.
var Roles = map[string]Role{
	"scout": {
		Name:    "scout",
		Scopes:  []string{"vacancies:write", "companies:write", "tools:call"},
		Handles: nil, // scout is driven by searches, not by the queue
	},
	"analyst": {
		Name:    "analyst",
		Scopes:  []string{"vacancies:read", "inventory:read", "scores:write", "tools:call"},
		Handles: []string{StepScoreVacancy},
	},
	"writer": {
		Name:    "writer",
		Scopes:  []string{"vacancies:read", "scores:read", "letters:write", "tools:call"},
		Handles: []string{StepDraftLetter},
	},
}

// ErrUnknownRole reports a role flag that is not one of the three.
var ErrUnknownRole = errors.New("unknown role")

// Lookup returns a role by name.
func Lookup(name string) (Role, error) {
	r, ok := Roles[name]
	if !ok {
		return Role{}, ErrUnknownRole
	}
	return r, nil
}

// Worker runs one role.
type Worker struct {
	Role  Role
	Agent runtime.Agent
	Queue queue.Queue
	Rt    *runtime.Runtime
	Log   *slog.Logger

	// Deadline is how long one step may take, and what the handover
	// envelope advertises.
	Deadline time.Duration
}

// New returns a worker for a role.
func New(role Role, agentID string, q queue.Queue, rt *runtime.Runtime, log *slog.Logger) *Worker {
	return &Worker{
		Role:     role,
		Agent:    runtime.Agent{ID: agentID, Name: role.Name, Scopes: role.Scopes},
		Queue:    q,
		Rt:       rt,
		Log:      log.With("role", role.Name),
		Deadline: 5 * time.Minute,
	}
}

// Step runs at most one step. It reports whether it found work, so a
// caller can back off when the queue is empty instead of spinning.
func (w *Worker) Step(ctx context.Context) (bool, error) {
	s, err := w.Queue.Claim(ctx, w.Agent.ID, w.Role.Handles)
	if errors.Is(err, queue.ErrNoWork) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	deadline := time.Now().Add(w.Deadline)
	env := queue.Handover(s, s.ToolName, w.Role.Name, deadline)
	w.Log.Info("claimed",
		"trace_id", env.TraceID, "task_id", env.TaskID, "step_id", env.StepID,
		"seq", env.Seq, "tool", env.Tool, "attempt", env.Attempt)

	// The step's identity goes into the context, so every call the tool
	// makes - including anything nested - is attributed to this trace,
	// this task and this step without the tool having to pass them on.
	runCtx, cancel := context.WithDeadline(runtime.Context(ctx, s.TraceID, s.TaskID, s.ID), deadline)
	defer cancel()

	// The step id is the idempotency key (SPEC.md §7), so a step retried
	// after its handler already finished replays instead of acting twice.
	out, execErr := w.Rt.Execute(runCtx, runtime.Request{
		Agent:          w.Agent,
		Tool:           s.ToolName,
		Input:          s.Input,
		IdempotencyKey: s.ID,
	})
	if execErr != nil {
		return true, w.fail(ctx, s, execErr)
	}

	if err := w.Queue.Complete(ctx, s.ID, out); err != nil {
		return true, err
	}
	w.Log.Info("done", "trace_id", s.TraceID, "step_id", s.ID, "tool", s.ToolName)
	return true, nil
}

// fail records a failure and says whether the step is finished with.
//
// A refusal the runtime made on policy - a missing scope, an argument
// that does not validate, a budget spent - will refuse again identically
// on the next attempt, so retrying it only burns attempts and delays the
// task reaching `dead`. Those go straight to dead; everything else gets
// the backoff.
func (w *Worker) fail(ctx context.Context, s *queue.Step, cause error) error {
	code := runtime.CodeOf(cause)
	if code == "" {
		code = "handler_failed"
	}

	if permanent(code) {
		w.Log.Warn("refused, not retrying",
			"trace_id", s.TraceID, "step_id", s.ID, "tool", s.ToolName,
			"error_code", code, "error", cause)
		return w.Queue.Kill(ctx, s.ID, code, cause)
	}

	dead, err := w.Queue.Fail(ctx, s.ID, code, cause)
	if err != nil {
		return err
	}
	w.Log.Warn("failed",
		"trace_id", s.TraceID, "step_id", s.ID, "tool", s.ToolName,
		"error_code", code, "attempt", s.Attempts, "dead", dead, "error", cause)
	return nil
}

// permanent reports a refusal that will not become true on a retry.
func permanent(code string) bool {
	switch code {
	case runtime.CodeForbidden, runtime.CodeInvalidInput, runtime.CodeInvalidOutput,
		runtime.CodeNotFound, runtime.CodeDepthExceeded, runtime.CodeCycleDetected,
		runtime.CodeBudgetExceeded:
		return true
	default:
		// handler_failed and timeout are worth another attempt: the first
		// may be transient, the second may be a slow dependency.
		return false
	}
}

// Run works the queue until the context ends, sleeping when it is empty.
func (w *Worker) Run(ctx context.Context, poll time.Duration) error {
	w.Log.Info("working the queue", "handles", w.Role.Handles, "poll", poll)

	for {
		worked, err := w.Step(ctx)
		if err != nil {
			// A failure to reach the queue is not a reason to exit: the
			// database may come back, and a role that quits leaves its
			// steps unclaimed until someone notices.
			w.Log.Error("step failed", "error", err)
		}
		if worked {
			continue // drain while there is work
		}

		select {
		case <-ctx.Done():
			w.Log.Info("stopping")
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// Handover queues the next role's work and is how a task moves on.
//
// analyst calls this after scoring; writer is what claims the result.
// Returning the step lets the caller log the envelope.
func Handover(ctx context.Context, q queue.Queue, taskID, tool string, input json.RawMessage) (*queue.Step, error) {
	return q.Enqueue(ctx, taskID, tool, input)
}
