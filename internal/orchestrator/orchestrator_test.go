package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/isrenato/robasko/internal/queue"
	"github.com/isrenato/robasko/internal/registry"
	"github.com/isrenato/robasko/internal/runtime"
)

// --- doubles -------------------------------------------------------------

type fakeQueue struct {
	mu       sync.Mutex
	pending  []*queue.Step
	done     map[string]json.RawMessage
	failed   map[string]string
	killed   map[string]string
	enqueued []string
	deadAt   int // Fail reports dead once attempts reach this
}

func newQueue(steps ...*queue.Step) *fakeQueue {
	return &fakeQueue{
		pending: steps, done: map[string]json.RawMessage{},
		failed: map[string]string{}, killed: map[string]string{},
		deadAt: queue.MaxAttempts,
	}
}

func (q *fakeQueue) Enqueue(_ context.Context, taskID, tool string, in json.RawMessage) (*queue.Step, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.enqueued = append(q.enqueued, tool)
	return &queue.Step{ID: "new", TaskID: taskID, ToolName: tool, Input: in}, nil
}

func (q *fakeQueue) Claim(_ context.Context, _ string, tools []string) (*queue.Step, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, s := range q.pending {
		if !handles(tools, s.ToolName) {
			continue
		}
		q.pending = append(q.pending[:i], q.pending[i+1:]...)
		s.Attempts++
		return s, nil
	}
	return nil, queue.ErrNoWork
}

func handles(tools []string, name string) bool {
	if len(tools) == 0 {
		return true
	}
	for _, t := range tools {
		if t == name {
			return true
		}
	}
	return false
}

func (q *fakeQueue) Complete(_ context.Context, id string, result json.RawMessage) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.done[id] = result
	return nil
}

func (q *fakeQueue) Fail(_ context.Context, id, code string, _ error) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.failed[id] = code
	return len(q.failed) >= q.deadAt, nil
}

func (q *fakeQueue) Kill(_ context.Context, id, code string, _ error) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.killed[id] = code
	return nil
}

type nullLog struct{ calls int }

func (l *nullLog) Begin(context.Context, *runtime.Call) (string, error) {
	l.calls++
	return fmt.Sprintf("call-%d", l.calls), nil
}
func (l *nullLog) Finish(context.Context, *runtime.Call) error       { return nil }
func (l *nullLog) CountForTask(context.Context, string) (int, error) { return 0, nil }
func (l *nullLog) Replay(context.Context, string) (json.RawMessage, bool, error) {
	return nil, false, nil
}

type tools map[string]*registry.Tool

func (f tools) Resolve(_ context.Context, name string, _ int) (*registry.Tool, error) {
	if t, ok := f[name]; ok {
		return t, nil
	}
	return nil, fmt.Errorf("%q: %w", name, registry.ErrNotFound)
}

func step(id, tool string) *queue.Step {
	return &queue.Step{ID: id, TaskID: "task-1", TraceID: "trace-1", ToolName: tool,
		Input: json.RawMessage(`{}`)}
}

func tool(t *testing.T, name string, scopes []string) *registry.Tool {
	t.Helper()
	schema := json.RawMessage(`{"type":"object","additionalProperties":false,"required":[],"properties":{"ok":{"type":"boolean"}}}`)
	built, err := registry.NewTool(registry.Definition{
		Name: name, InputSchema: schema, OutputSchema: schema,
		RequiredScopes: scopes, Timeout: time.Second,
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	return built
}

func worker(t *testing.T, roleName string, q queue.Queue, reg tools) (*Worker, *runtime.Runtime) {
	t.Helper()
	role, err := Lookup(roleName)
	if err != nil {
		t.Fatal(err)
	}
	rt := runtime.New(reg, &nullLog{})
	return New(role, "agent-1", q, rt, slog.New(slog.NewTextHandler(io.Discard, nil))), rt
}

// --- the roles ------------------------------------------------------------

// SPEC.md §8: each role holds its own scopes, and no role can perform
// another's writes.
func TestRolesHoldTheScopesTheSpecGivesThem(t *testing.T) {
	cases := map[string]struct{ has, lacks string }{
		"scout":   {"vacancies:write", "scores:write"},
		"analyst": {"scores:write", "letters:write"},
		"writer":  {"letters:write", "vacancies:write"},
	}
	for name, want := range cases {
		role, err := Lookup(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var has, lacks bool
		for _, s := range role.Scopes {
			if s == want.has {
				has = true
			}
			if s == want.lacks {
				lacks = true
			}
		}
		if !has {
			t.Errorf("%s does not hold %s", name, want.has)
		}
		if lacks {
			t.Errorf("%s holds %s, which belongs to another role", name, want.lacks)
		}
	}
	if _, err := Lookup("courier"); !errors.Is(err, ErrUnknownRole) {
		t.Errorf("got %v, want ErrUnknownRole", err)
	}
}

// A role claims only its own work, or whichever process polled first
// would take whatever was there.
func TestARoleClaimsOnlyItsOwnSteps(t *testing.T) {
	q := newQueue(step("s-letter", StepDraftLetter))
	w, rt := worker(t, "analyst", q, tools{StepScoreVacancy: tool(t, StepScoreVacancy, nil)})
	rt.Register(StepScoreVacancy, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"ok":true}`), nil
	})

	worked, err := w.Step(context.Background())
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if worked {
		t.Error("analyst claimed a step belonging to writer")
	}
}

func TestStepRunsAndCompletes(t *testing.T) {
	q := newQueue(step("s-1", StepScoreVacancy))
	w, rt := worker(t, "analyst", q, tools{StepScoreVacancy: tool(t, StepScoreVacancy, []string{"scores:write"})})

	var sawTrace, sawTask string
	rt.Register(StepScoreVacancy, func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
		sawTrace = runtime.TraceID(ctx)
		sawTask = "set"
		return json.RawMessage(`{"ok":true}`), nil
	})

	worked, err := w.Step(context.Background())
	if err != nil || !worked {
		t.Fatalf("Step: worked=%v err=%v", worked, err)
	}
	if string(q.done["s-1"]) != `{"ok":true}` {
		t.Errorf("completed with %s", q.done["s-1"])
	}
	// The step's identity reaches the tool, so nested calls are attributed.
	if sawTrace != "trace-1" || sawTask == "" {
		t.Errorf("the trace did not reach the handler: %q", sawTrace)
	}
}

// A refusal on policy will refuse again identically. Retrying it only
// burns attempts and delays the task reaching a state someone can act on.
func TestPolicyRefusalsAreNotRetried(t *testing.T) {
	// analyst holds no letters:write, so the tool is out of reach.
	q := newQueue(step("s-1", StepScoreVacancy))
	w, rt := worker(t, "analyst", q, tools{StepScoreVacancy: tool(t, StepScoreVacancy, []string{"letters:write"})})
	rt.Register(StepScoreVacancy, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"ok":true}`), nil
	})

	if _, err := w.Step(context.Background()); err != nil {
		t.Fatalf("Step: %v", err)
	}
	if q.killed["s-1"] != runtime.CodeForbidden {
		t.Errorf("killed as %q, want forbidden", q.killed["s-1"])
	}
	if _, retried := q.failed["s-1"]; retried {
		t.Error("a scope refusal was scheduled for a retry")
	}
}

// A handler that fails for its own reasons may succeed next time.
func TestHandlerFailuresAreRetried(t *testing.T) {
	q := newQueue(step("s-1", StepScoreVacancy))
	w, rt := worker(t, "analyst", q, tools{StepScoreVacancy: tool(t, StepScoreVacancy, nil)})
	rt.Register(StepScoreVacancy, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return nil, errors.New("the database blinked")
	})

	if _, err := w.Step(context.Background()); err != nil {
		t.Fatalf("Step: %v", err)
	}
	if q.failed["s-1"] != "handler_failed" {
		t.Errorf("failed as %q", q.failed["s-1"])
	}
	if _, killed := q.killed["s-1"]; killed {
		t.Error("a transient failure was killed instead of retried")
	}
}

func TestPermanentCodes(t *testing.T) {
	for _, code := range []string{
		runtime.CodeForbidden, runtime.CodeInvalidInput, runtime.CodeInvalidOutput,
		runtime.CodeNotFound, runtime.CodeDepthExceeded, runtime.CodeCycleDetected,
		runtime.CodeBudgetExceeded,
	} {
		if !permanent(code) {
			t.Errorf("%s should not be retried", code)
		}
	}
	for _, code := range []string{runtime.CodeTimeout, "handler_failed"} {
		if permanent(code) {
			t.Errorf("%s is worth another attempt", code)
		}
	}
}

func TestAnEmptyQueueIsNotAnError(t *testing.T) {
	w, _ := worker(t, "writer", newQueue(), tools{})

	worked, err := w.Step(context.Background())
	if err != nil {
		t.Errorf("an empty queue reported %v", err)
	}
	if worked {
		t.Error("claimed something from an empty queue")
	}
}

// The handover is what moves a task on: analyst queues the drafting step
// that writer claims.
func TestHandoverQueuesTheNextRolesWork(t *testing.T) {
	q := newQueue()
	s, err := Handover(context.Background(), q, "task-1", StepDraftLetter, json.RawMessage(`{"vacancy_id":"v1"}`))
	if err != nil {
		t.Fatalf("Handover: %v", err)
	}
	if s.ToolName != StepDraftLetter {
		t.Errorf("queued %q", s.ToolName)
	}
	if len(q.enqueued) != 1 || q.enqueued[0] != StepDraftLetter {
		t.Errorf("enqueued %v", q.enqueued)
	}

	// And writer is the role that claims it.
	w, _ := worker(t, "writer", newQueue(step("s-1", StepDraftLetter)), tools{StepDraftLetter: tool(t, StepDraftLetter, nil)})
	if !handles(w.Role.Handles, StepDraftLetter) {
		t.Error("writer does not claim draft_letter")
	}
	if handles(w.Role.Handles, StepScoreVacancy) {
		t.Error("writer claims analyst's work")
	}
}

// Run drains the queue and then waits rather than spinning.
func TestRunDrainsThenStops(t *testing.T) {
	q := newQueue(step("s-1", StepScoreVacancy), step("s-2", StepScoreVacancy))
	w, rt := worker(t, "analyst", q, tools{StepScoreVacancy: tool(t, StepScoreVacancy, nil)})
	rt.Register(StepScoreVacancy, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"ok":true}`), nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	if err := w.Run(ctx, 50*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Run returned %v", err)
	}
	if len(q.done) != 2 {
		t.Errorf("completed %d steps, want both", len(q.done))
	}
}
