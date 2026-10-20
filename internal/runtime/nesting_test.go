package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// nest returns a handler that reaches another tool the way the loop does:
// by executing call_tool, not by reaching into its handler. Going through
// Execute is what puts call_tool in the log and on the stack, and it is
// also what costs a level of depth - a nested call is two frames, the
// nesting tool and the tool it reached.
func nest(r *Runtime, caller Agent, target string) Handler {
	return func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
		args, _ := json.Marshal(map[string]any{"tool": target, "input": map[string]any{}})
		return r.Execute(ctx, Request{Agent: caller, Tool: CallToolName, Input: args})
	}
}

func chain(t *testing.T, names ...string) (*Runtime, *fakeLog, Agent) {
	t.Helper()
	tools := fakeTools{CallToolName: tool(t, CallToolName, nil, 5*time.Second)}
	for _, n := range names {
		tools[n] = tool(t, n, nil, 5*time.Second)
	}
	r, log := setup(t, tools)
	return r, log, agent()
}

// A nested call is attributed: each frame records its parent and its
// depth, which is what lets a trace be drawn as a tree (SPEC.md §17).
//
// Note the shape. One nesting costs two frames - call_tool and the tool
// it reached - so with max_depth 3 a chain fits two nested tools, not
// three. The skill in SPEC.md §9 is not a chain: it is one parent calling
// several tools as siblings, each two frames down, which fits.
func TestNestedCallsRecordParentAndDepth(t *testing.T) {
	r, log, a := chain(t, "a", "b")
	r.Register(CallToolName, r.CallTool(a))
	r.Register("a", nest(r, a, "b"))
	r.Register("b", ok)

	if _, err := r.Execute(taskCtx(), Request{Agent: a, Tool: "a", Input: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	for _, want := range []struct {
		tool  string
		depth int
	}{{"a", 0}, {CallToolName, 1}, {"b", 2}} {
		c := log.find(want.tool)
		if c == nil {
			t.Fatalf("%s was not logged", want.tool)
		}
		if c.Depth != want.depth {
			t.Errorf("%s logged at depth %d, want %d", want.tool, c.Depth, want.depth)
		}
		if want.depth > 0 && c.ParentCallID == "" {
			t.Errorf("%s has no parent", want.tool)
		}
	}
	if c := log.find("a"); c.TraceID != "trace-1" {
		t.Errorf("the trace id did not reach the chain: %q", c.TraceID)
	}
}

// SPEC.md §7: max_depth 3. The chain is stopped rather than allowed to
// run away, and the refusal keeps its own code all the way back out.
func TestDepthIsBounded(t *testing.T) {
	r, _, a := chain(t, "a", "b", "c")
	r.Register(CallToolName, r.CallTool(a))
	r.Register("a", nest(r, a, "b"))
	r.Register("b", nest(r, a, "c"))
	r.Register("c", ok)

	_, err := r.Execute(taskCtx(), Request{Agent: a, Tool: "a", Input: json.RawMessage(`{}`)})
	if CodeOf(err) != CodeDepthExceeded {
		t.Fatalf("got %v, want depth_exceeded", err)
	}
}

// The same tool twice in one stack is a cycle; the same tool twice in
// sequence is ordinary. The distinction is the whole point of tracking a
// stack rather than a count.
func TestCycleIsDetected(t *testing.T) {
	r, _, a := chain(t, "a")
	r.Register(CallToolName, r.CallTool(a))
	r.Register("a", nest(r, a, "a")) // straight back to itself

	_, err := r.Execute(taskCtx(), Request{Agent: a, Tool: "a", Input: json.RawMessage(`{}`)})
	if CodeOf(err) != CodeCycleDetected {
		t.Fatalf("got %v, want cycle_detected", err)
	}
}

// Worth knowing rather than discovering later: under SPEC.md §7's limits
// the depth check shadows the cycle check for anything but a self-call.
// One nesting costs two frames, so a -> b -> a needs four and is refused
// at depth 3 before the stack is consulted.
//
// That leaves cycle detection covering the direct case and standing by
// for the longer ones if max_depth is ever raised. It is not dead code,
// but it is not the limit doing the work either, and a trace of a looping
// chain will say depth_exceeded rather than cycle_detected.
func TestDepthShadowsLongerCycles(t *testing.T) {
	r, _, a := chain(t, "a", "b")
	r.Register(CallToolName, r.CallTool(a))
	r.Register("a", nest(r, a, "b"))
	r.Register("b", nest(r, a, "a")) // would be a cycle, never reached

	_, err := r.Execute(taskCtx(), Request{Agent: a, Tool: "a", Input: json.RawMessage(`{}`)})
	if CodeOf(err) != CodeDepthExceeded {
		t.Fatalf("got %v, want depth_exceeded to fire first", err)
	}
}

func TestSiblingCallsAreNotACycle(t *testing.T) {
	r, log, a := chain(t, "parent", "child")
	r.Register(CallToolName, r.CallTool(a))
	r.Register("child", ok)
	r.Register("parent", func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
		// The same tool twice from one parent: two siblings, not a cycle.
		// This is the shape a skill has (SPEC.md §9).
		for i := 0; i < 2; i++ {
			args, _ := json.Marshal(map[string]any{"tool": "child", "input": map[string]any{}})
			if _, err := r.Execute(ctx, Request{Agent: a, Tool: CallToolName, Input: args}); err != nil {
				return nil, err
			}
		}
		return json.RawMessage(`{"done":true}`), nil
	})

	if _, err := r.Execute(taskCtx(), Request{Agent: a, Tool: "parent", Input: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("two sibling calls were refused: %v", err)
	}

	var children int
	log.mu.Lock()
	for _, c := range log.calls {
		if c.Tool == "child" {
			children++
		}
	}
	log.mu.Unlock()
	if children != 2 {
		t.Errorf("logged %d child calls, want 2", children)
	}
}

func TestCallToolCannotCallItself(t *testing.T) {
	r, _, a := chain(t)
	r.Register(CallToolName, r.CallTool(a))

	args, _ := json.Marshal(map[string]any{"tool": CallToolName, "input": map[string]any{}})
	_, err := r.CallTool(a)(taskCtx(), args)
	if CodeOf(err) != CodeCycleDetected {
		t.Errorf("got %v, want cycle_detected", err)
	}
}

// Nesting must not widen what an agent can reach. A tool the caller could
// not call directly stays out of reach through call_tool.
func TestNestingDoesNotWidenScope(t *testing.T) {
	tools := fakeTools{
		CallToolName:   tool(t, CallToolName, nil, time.Second),
		"letter.draft": tool(t, "letter.draft", []string{"letters:write"}, time.Second),
		"opener":       tool(t, "opener", nil, time.Second),
	}
	r, _ := setup(t, tools)
	scout := Agent{ID: "s-1", Name: "scout", Scopes: []string{"vacancies:write"}}

	r.Register(CallToolName, r.CallTool(scout))
	r.Register("letter.draft", ok)
	r.Register("opener", nest(r, scout, "letter.draft"))

	_, err := r.Execute(taskCtx(), Request{Agent: scout, Tool: "opener", Input: json.RawMessage(`{}`)})
	if CodeOf(err) != CodeForbidden {
		t.Fatalf("got %v, want forbidden: scout reached a writer's tool by nesting", err)
	}
}

// SPEC.md §7: a per-task budget of 50 calls, counted across the task
// rather than per chain, because sibling agents share one.
func TestBudgetIsEnforcedPerTask(t *testing.T) {
	r, log, a := chain(t, "x")
	r.Register("x", ok)
	log.perTask["task-1"] = MaxCalls

	_, err := r.Execute(taskCtx(), Request{Agent: a, Tool: "x", Input: json.RawMessage(`{}`)})
	if CodeOf(err) != CodeBudgetExceeded {
		t.Fatalf("got %v, want budget_exceeded", err)
	}
}

func TestBudgetDoesNotApplyWithoutATask(t *testing.T) {
	r, log, a := chain(t, "x")
	r.Register("x", ok)
	log.perTask[""] = MaxCalls + 10

	// A call outside a task - an operator running a tool by hand - has no
	// budget to exceed.
	if _, err := r.Execute(context.Background(), Request{Agent: a, Tool: "x", Input: json.RawMessage(`{}`)}); err != nil {
		t.Errorf("a call outside a task was refused: %v", err)
	}
}

// A nested refusal keeps its own code on the way out, so a trace shows
// the limit that stopped the chain rather than a generic failure.
func TestNestedRefusalKeepsItsCode(t *testing.T) {
	r, log, a := chain(t, "outer", "inner")
	r.Register(CallToolName, r.CallTool(a))
	r.Register("outer", nest(r, a, "missing"))

	_, err := r.Execute(taskCtx(), Request{Agent: a, Tool: "outer", Input: json.RawMessage(`{}`)})
	if CodeOf(err) != CodeNotFound {
		t.Fatalf("got %v, want not_found to survive the nesting", err)
	}
	if c := log.find("outer"); c == nil || c.ErrorCode != CodeNotFound {
		t.Errorf("the outer call logged %v", c)
	}
}

// Depth is readable by a handler that wants to log it, without the
// handler being handed anything it could tamper with.
func TestDepthIsReadableFromTheContext(t *testing.T) {
	r, _, a := chain(t, "a", "b")
	seen := map[string]int{}

	r.Register(CallToolName, r.CallTool(a))
	r.Register("a", func(ctx context.Context, in json.RawMessage) (json.RawMessage, error) {
		seen["a"] = Depth(ctx)
		return nest(r, a, "b")(ctx, in)
	})
	r.Register("b", func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
		seen["b"] = Depth(ctx)
		return json.RawMessage(`{"done":true}`), nil
	})

	if _, err := r.Execute(taskCtx(), Request{Agent: a, Tool: "a", Input: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// a runs one frame below its own call; b runs three, because
	// call_tool sits between them.
	if seen["a"] != 1 || seen["b"] != 3 {
		t.Errorf("depths seen by handlers: %v, want a=1 b=3 (a, call_tool, b)", seen)
	}
	if TraceID(taskCtx()) != "trace-1" {
		t.Error("the trace id is not readable from the context")
	}
}

var _ = fmt.Sprintf
