package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/isrenato/robasko/internal/registry"
)

// --- doubles -------------------------------------------------------------

type fakeLog struct {
	mu       sync.Mutex
	calls    []Call
	replays  map[string]json.RawMessage
	perTask  map[string]int
	beginErr error
}

func newLog() *fakeLog {
	return &fakeLog{replays: map[string]json.RawMessage{}, perTask: map[string]int{}}
}

func (l *fakeLog) Begin(_ context.Context, c *Call) (string, error) {
	if l.beginErr != nil {
		return "", l.beginErr
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	id := fmt.Sprintf("call-%d", len(l.calls)+1)
	c.ID = id
	l.calls = append(l.calls, *c)
	l.perTask[c.TaskID]++
	return id, nil
}

func (l *fakeLog) Finish(_ context.Context, c *Call) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.calls {
		if l.calls[i].ID == c.ID {
			l.calls[i] = *c
			return nil
		}
	}
	return fmt.Errorf("no call %s", c.ID)
}

func (l *fakeLog) CountForTask(_ context.Context, taskID string) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.perTask[taskID], nil
}

func (l *fakeLog) Replay(_ context.Context, key string) (json.RawMessage, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out, ok := l.replays[key]
	return out, ok, nil
}

func (l *fakeLog) find(tool string) *Call {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.calls {
		if l.calls[i].Tool == tool {
			return &l.calls[i]
		}
	}
	return nil
}

type fakeTools map[string]*registry.Tool

func (f fakeTools) Resolve(_ context.Context, name string, _ int) (*registry.Tool, error) {
	if t, ok := f[name]; ok {
		return t, nil
	}
	return nil, fmt.Errorf("%q: %w", name, registry.ErrNotFound)
}

// tool builds a registered tool. Its schemas name the properties the
// tests use, because the registry's policy requires
// additionalProperties:false and a tool built in code is held to it just
// as a stored one is.
func tool(t *testing.T, name string, scopes []string, timeout time.Duration) *registry.Tool {
	t.Helper()
	schema := json.RawMessage(`{
		"type":"object","additionalProperties":false,"required":[],
		"properties":{
			"vacancy_id":{"type":"string"},
			"done":{"type":"boolean"},
			"already":{"type":"string"},
			"tool":{"type":"string"},
			"version":{"type":"integer"},
			"input":{"type":"object"},
			"output":{"type":"object"},
			"n":{"type":"integer"}
		}}`)
	built, err := registry.NewTool(registry.Definition{
		Name: name, InputSchema: schema, OutputSchema: schema,
		RequiredScopes: scopes, Timeout: timeout,
	}, 1)
	if err != nil {
		t.Fatalf("building %s: %v", name, err)
	}
	return built
}

func agent(scopes ...string) Agent {
	return Agent{ID: "a-1", Name: "analyst", Scopes: scopes}
}

func ok(_ context.Context, in json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{"done":true}`), nil
}

func setup(t *testing.T, tools fakeTools) (*Runtime, *fakeLog) {
	t.Helper()
	log := newLog()
	return New(tools, log), log
}

func taskCtx() context.Context {
	return Context(context.Background(), "trace-1", "task-1", "step-1")
}

// --- the happy path and the log ------------------------------------------

func TestExecuteRunsAndRecordsTheCall(t *testing.T) {
	r, log := setup(t, fakeTools{"score.compute": tool(t, "score.compute", []string{"scores:write"}, time.Second)})
	r.Register("score.compute", ok)

	out, err := r.Execute(taskCtx(), Request{
		Agent: agent("scores:write"), Tool: "score.compute", Input: json.RawMessage(`{"vacancy_id":"v1"}`),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if string(out) != `{"done":true}` {
		t.Errorf("output %s", out)
	}

	c := log.find("score.compute")
	if c == nil {
		t.Fatal("the call was not logged")
	}
	if c.Status != "ok" || c.ErrorCode != "" {
		t.Errorf("logged as %q / %q", c.Status, c.ErrorCode)
	}
	// The trace is what reconstructs a run (SPEC.md 17).
	if c.TraceID != "trace-1" || c.TaskID != "task-1" || c.StepID != "step-1" {
		t.Errorf("identity not carried: %+v", c)
	}
	if c.Depth != 0 || c.ParentCallID != "" {
		t.Errorf("a top-level call has depth %d parent %q", c.Depth, c.ParentCallID)
	}
}

// --- the refusals ---------------------------------------------------------

// SPEC.md 8: scout cannot score, analyst cannot write letters. The scope
// check is what makes that true rather than a convention.
func TestScopeIsEnforced(t *testing.T) {
	r, log := setup(t, fakeTools{"letter.draft": tool(t, "letter.draft", []string{"letters:write"}, time.Second)})
	r.Register("letter.draft", ok)

	_, err := r.Execute(taskCtx(), Request{
		Agent: agent("vacancies:read", "scores:write"), Tool: "letter.draft", Input: json.RawMessage(`{}`),
	})
	if CodeOf(err) != CodeForbidden {
		t.Fatalf("got %v, want forbidden", err)
	}
	// A refusal on scope must not leave a row implying the tool ran.
	if log.find("letter.draft") != nil {
		t.Error("a forbidden call was written to the log")
	}
}

func TestInputIsValidatedBeforeTheHandler(t *testing.T) {
	strict, err := registry.NewTool(registry.Definition{
		Name:         "vacancy.get",
		InputSchema:  json.RawMessage(`{"type":"object","additionalProperties":false,"required":["vacancy_id"],"properties":{"vacancy_id":{"type":"string"}}}`),
		OutputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":[],"properties":{}}`),
	}, 1)
	if err != nil {
		t.Fatal(err)
	}

	ran := false
	r, log := setup(t, fakeTools{"vacancy.get": strict})
	r.Register("vacancy.get", func(context.Context, json.RawMessage) (json.RawMessage, error) {
		ran = true
		return json.RawMessage(`{}`), nil
	})

	_, execErr := r.Execute(taskCtx(), Request{
		Agent: agent(), Tool: "vacancy.get", Input: json.RawMessage(`{"vacancy_id":42}`),
	})
	if CodeOf(execErr) != CodeInvalidInput {
		t.Fatalf("got %v, want invalid_input", execErr)
	}
	if ran {
		t.Error("the handler ran on input that did not validate")
	}
	if log.find("vacancy.get") != nil {
		t.Error("a call rejected on input was logged as made")
	}
}

// A handler that drifts from its contract is caught here, not by whoever
// consumes the result.
func TestOutputIsValidated(t *testing.T) {
	strict, err := registry.NewTool(registry.Definition{
		Name:         "score.compute",
		InputSchema:  json.RawMessage(`{"type":"object","additionalProperties":false,"required":[],"properties":{}}`),
		OutputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["score"],"properties":{"score":{"type":"number"}}}`),
	}, 1)
	if err != nil {
		t.Fatal(err)
	}

	r, log := setup(t, fakeTools{"score.compute": strict})
	r.Register("score.compute", func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"score":"high"}`), nil
	})

	_, execErr := r.Execute(taskCtx(), Request{Agent: agent(), Tool: "score.compute", Input: json.RawMessage(`{}`)})
	if CodeOf(execErr) != CodeInvalidOutput {
		t.Fatalf("got %v, want invalid_output", execErr)
	}
	// The attempt is logged: the tool did run, and the trace should say so.
	c := log.find("score.compute")
	if c == nil || c.ErrorCode != CodeInvalidOutput {
		t.Errorf("logged as %+v", c)
	}
}

func TestUnknownToolIsNotFound(t *testing.T) {
	r, _ := setup(t, fakeTools{})
	_, err := r.Execute(taskCtx(), Request{Agent: agent(), Tool: "nope", Input: json.RawMessage(`{}`)})
	if CodeOf(err) != CodeNotFound {
		t.Errorf("got %v, want not_found", err)
	}
}

func TestTimeoutIsRecorded(t *testing.T) {
	r, log := setup(t, fakeTools{"slow": tool(t, "slow", nil, 20*time.Millisecond)})
	r.Register("slow", func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})

	_, err := r.Execute(taskCtx(), Request{Agent: agent(), Tool: "slow", Input: json.RawMessage(`{}`)})
	if CodeOf(err) != CodeTimeout {
		t.Fatalf("got %v, want timeout", err)
	}
	if c := log.find("slow"); c == nil || c.ErrorCode != CodeTimeout {
		t.Errorf("logged as %+v", c)
	}
}

// --- idempotency ----------------------------------------------------------

// Queue work keys on step_id, so a retried step replays rather than acting
// twice (SPEC.md 7).
func TestIdempotentKeyReplaysWithoutRunningTheHandler(t *testing.T) {
	r, log := setup(t, fakeTools{"letter.draft": tool(t, "letter.draft", nil, time.Second)})
	log.replays["step-9"] = json.RawMessage(`{"already":"done"}`)

	ran := false
	r.Register("letter.draft", func(context.Context, json.RawMessage) (json.RawMessage, error) {
		ran = true
		return json.RawMessage(`{}`), nil
	})

	out, err := r.Execute(taskCtx(), Request{
		Agent: agent(), Tool: "letter.draft", Input: json.RawMessage(`{}`), IdempotencyKey: "step-9",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if string(out) != `{"already":"done"}` {
		t.Errorf("got %s, want the stored output", out)
	}
	if ran {
		t.Error("the handler ran despite a finished call under the same key")
	}
}

func TestUnknownKeyRunsNormally(t *testing.T) {
	r, _ := setup(t, fakeTools{"x": tool(t, "x", nil, time.Second)})
	r.Register("x", ok)

	if _, err := r.Execute(taskCtx(), Request{
		Agent: agent(), Tool: "x", Input: json.RawMessage(`{}`), IdempotencyKey: "fresh",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}
