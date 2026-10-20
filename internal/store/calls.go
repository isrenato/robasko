package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/isrenato/robasko/internal/runtime"
)

// CallLog records tool calls in tool_calls. It is the runtime.Recorder the
// runtime writes through, and the table GET /v1/traces/{id} reads back.
//
// A call is written before its handler runs and finished after, which is
// what makes an interrupted call visible as `running` instead of absent.
// A trace missing its failures is worse than no trace.
type CallLog struct{ s *Store }

// Calls returns the call log over this store.
func (s *Store) Calls() *CallLog { return &CallLog{s: s} }

// Begin writes a call before its handler runs.
func (l *CallLog) Begin(ctx context.Context, c *runtime.Call) (string, error) {
	var id string
	err := l.s.pool.QueryRow(ctx, `
		INSERT INTO tool_calls
			(trace_id, task_id, step_id, parent_call_id, depth, agent_id,
			 tool_name, tool_version, input, status, idempotency_key, started_at)
		VALUES ($1, NULLIF($2,'')::uuid, NULLIF($3,'')::uuid, NULLIF($4,'')::uuid,
		        $5, NULLIF($6,'')::uuid, $7, NULLIF($8, 0), $9, 'running',
		        NULLIF($10,''), $11)
		RETURNING id`,
		c.TraceID, c.TaskID, c.StepID, c.ParentCallID, c.Depth, c.AgentID,
		c.Tool, c.Version, nullJSON(c.Input), c.StepID, c.StartedAt,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store: beginning the call to %s: %w", c.Tool, err)
	}
	return id, nil
}

// Finish closes a call with its outcome.
func (l *CallLog) Finish(ctx context.Context, c *runtime.Call) error {
	_, err := l.s.pool.Exec(ctx, `
		UPDATE tool_calls
		SET output = $2, status = $3, error_code = NULLIF($4,''), duration_ms = $5
		WHERE id = $1`,
		c.ID, nullJSON(c.Output), c.Status, c.ErrorCode, c.Duration.Milliseconds())
	if err != nil {
		return fmt.Errorf("store: finishing the call to %s: %w", c.Tool, err)
	}
	return nil
}

// CountForTask returns how many calls a task has made, for the budget.
//
// Counted in the table rather than in memory because SPEC.md §8 runs
// three agents against one task: a per-process counter would let each of
// them spend the whole budget.
func (l *CallLog) CountForTask(ctx context.Context, taskID string) (int, error) {
	if taskID == "" {
		return 0, nil
	}
	var n int
	if err := l.s.pool.QueryRow(ctx,
		`SELECT count(*) FROM tool_calls WHERE task_id = $1::uuid`, taskID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: counting calls for task %s: %w", taskID, err)
	}
	return n, nil
}

// Replay returns a finished call's output for an idempotency key.
//
// Only a finished call replays. A call still running is one someone else
// is making right now, and returning its absent output as a result would
// be worse than running the handler twice.
func (l *CallLog) Replay(ctx context.Context, key string) (json.RawMessage, bool, error) {
	if key == "" {
		return nil, false, nil
	}
	var out []byte
	err := l.s.pool.QueryRow(ctx, `
		SELECT output FROM tool_calls
		WHERE idempotency_key = $1 AND status = 'ok'
		ORDER BY started_at DESC
		LIMIT 1`, key).Scan(&out)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: replaying %s: %w", key, err)
	}
	if out == nil {
		return nil, false, nil
	}
	return json.RawMessage(out), true, nil
}

// TraceNode is one call in a reconstructed trace.
type TraceNode struct {
	ID         string          `json:"id"`
	ParentID   string          `json:"parent_id,omitempty"`
	Depth      int             `json:"depth"`
	Tool       string          `json:"tool"`
	Version    int             `json:"version,omitempty"`
	Agent      string          `json:"agent,omitempty"`
	Status     string          `json:"status"`
	ErrorCode  string          `json:"error_code,omitempty"`
	DurationMS int             `json:"duration_ms"`
	Output     json.RawMessage `json:"-"`
}

// Trace returns every call under one trace id, in the order they started.
// The parent links make it a tree; SPEC.md §17 serves this as the call
// tree for one request.
func (l *CallLog) Trace(ctx context.Context, traceID string) ([]TraceNode, error) {
	rows, err := l.s.pool.Query(ctx, `
		SELECT c.id, coalesce(c.parent_call_id::text, ''), c.depth, c.tool_name,
		       coalesce(c.tool_version, 0), coalesce(a.name, ''), c.status,
		       coalesce(c.error_code, ''), coalesce(c.duration_ms, 0)
		FROM tool_calls c
		LEFT JOIN agents a ON a.id = c.agent_id
		WHERE c.trace_id = $1
		ORDER BY c.started_at, c.depth`, traceID)
	if err != nil {
		return nil, fmt.Errorf("store: reading trace %s: %w", traceID, err)
	}
	defer rows.Close()

	var out []TraceNode
	for rows.Next() {
		var n TraceNode
		if err := rows.Scan(&n.ID, &n.ParentID, &n.Depth, &n.Tool, &n.Version,
			&n.Agent, &n.Status, &n.ErrorCode, &n.DurationMS); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func nullJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return []byte(raw)
}
