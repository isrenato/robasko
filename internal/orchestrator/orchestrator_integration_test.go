package orchestrator

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/isrenato/robasko/internal/queue"
	"github.com/isrenato/robasko/internal/registry"
	"github.com/isrenato/robasko/internal/runtime"
	"github.com/isrenato/robasko/internal/store"
)

// liveWorker wires a role to the real queue, registry and call log, which
// is the combination agentd runs and the one the unit tests stub out.
func liveWorker(t *testing.T, roleName string) (*Worker, *queue.Postgres, *store.Store, *runtime.Runtime) {
	t.Helper()
	dsn := os.Getenv("ROBASKO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ROBASKO_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()

	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	role, err := Lookup(roleName)
	if err != nil {
		t.Fatal(err)
	}
	agentID, err := st.EnsureAgent(ctx, role.Name, role.Scopes)
	if err != nil {
		t.Fatalf("EnsureAgent: %v", err)
	}

	q := queue.NewPostgres(pool)
	rt := runtime.New(registry.New(pool), st.Calls())
	w := New(role, agentID, q, rt, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return w, q, st, rt
}

func liveTask(t *testing.T, q *queue.Postgres, traceID string) string {
	t.Helper()
	id, err := q.CreateTask(context.Background(), traceID, t.Name(), "", "")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	return id
}

// The whole path: a step is claimed off the real queue, its tool runs
// through the real runtime, the call lands in the real log, and the step
// and its task close.
func TestWorkerRunsAStepEndToEnd(t *testing.T) {
	w, q, st, rt := liveWorker(t, "analyst")
	ctx := context.Background()

	stamp := time.Now().Format("150405.000000000")
	traceID := "trace-live-" + stamp
	taskID := liveTask(t, q, traceID)

	toolName := StepScoreVacancy + "." + stamp
	schema := json.RawMessage(`{"type":"object","additionalProperties":false,"required":[],"properties":{"score":{"type":"number"}}}`)
	if _, err := registry.New(st.Pool()).Register(ctx, registry.Definition{
		Name: toolName, InputSchema: schema, OutputSchema: schema,
		RequiredScopes: []string{"scores:write"},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	rt.Register(toolName, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"score":83}`), nil
	})

	// The role claims by tool name, so point it at this test's tool.
	w.Role.Handles = []string{toolName}

	if _, err := q.Enqueue(ctx, taskID, toolName, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	worked, err := w.Step(ctx)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if !worked {
		t.Fatal("the worker found no work")
	}

	var stepStatus, taskStatus string
	if err := st.Pool().QueryRow(ctx, `
		SELECT s.status, t.status FROM task_steps s JOIN tasks t ON t.id = s.task_id
		WHERE s.task_id = $1`, taskID).Scan(&stepStatus, &taskStatus); err != nil {
		t.Fatal(err)
	}
	if stepStatus != queue.Done || taskStatus != "done" {
		t.Errorf("step %q task %q, want both done", stepStatus, taskStatus)
	}

	// SPEC.md §17: the trace reconstructs the run.
	trace, err := st.Calls().Trace(ctx, traceID)
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}
	if len(trace) != 1 {
		t.Fatalf("the trace has %d calls, want 1", len(trace))
	}
	if trace[0].Tool != toolName || trace[0].Status != "ok" {
		t.Errorf("logged %+v", trace[0])
	}
	if trace[0].Agent != "analyst" {
		t.Errorf("the call is attributed to %q, want analyst", trace[0].Agent)
	}
}

// A step whose tool the role cannot reach is killed rather than retried,
// and its task closes with the reason - which is what the daily report
// reads.
func TestScopeRefusalClosesTheTask(t *testing.T) {
	w, q, st, rt := liveWorker(t, "analyst")
	ctx := context.Background()

	stamp := time.Now().Format("150405.000000000")
	taskID := liveTask(t, q, "trace-refuse-"+stamp)

	toolName := "letters.only." + stamp
	schema := json.RawMessage(`{"type":"object","additionalProperties":false,"required":[],"properties":{}}`)
	if _, err := registry.New(st.Pool()).Register(ctx, registry.Definition{
		Name: toolName, InputSchema: schema, OutputSchema: schema,
		// analyst does not hold this.
		RequiredScopes: []string{"letters:write"},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	rt.Register(toolName, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		t.Error("the handler ran despite the missing scope")
		return json.RawMessage(`{}`), nil
	})

	w.Role.Handles = []string{toolName}
	if _, err := q.Enqueue(ctx, taskID, toolName, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if _, err := w.Step(ctx); err != nil {
		t.Fatalf("Step: %v", err)
	}

	var stepStatus, errorCode, taskStatus, reason string
	if err := st.Pool().QueryRow(ctx, `
		SELECT s.status, coalesce(s.error_code,''), t.status, coalesce(t.reason,'')
		FROM task_steps s JOIN tasks t ON t.id = s.task_id WHERE s.task_id = $1`,
		taskID).Scan(&stepStatus, &errorCode, &taskStatus, &reason); err != nil {
		t.Fatal(err)
	}
	if stepStatus != queue.Dead {
		t.Errorf("step %q, want dead without further attempts", stepStatus)
	}
	if errorCode != runtime.CodeForbidden {
		t.Errorf("error code %q, want forbidden", errorCode)
	}
	if taskStatus != "failed" || reason == "" {
		t.Errorf("task %q reason %q", taskStatus, reason)
	}
}
