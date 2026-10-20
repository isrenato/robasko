// Package runtime executes tools, including tools that call other tools.
//
// SPEC.md §2: no agent framework as a dependency, the loop is written
// here, and that is the point. What makes it a runtime rather than a
// function call is what it refuses: a caller without the scope, an
// argument that does not match the schema, a chain that goes too deep,
// a task that has spent its budget, a tool that calls itself round a
// cycle, and a step replayed after it already finished.
//
// Every limit lives here rather than in a prompt. A prompt can be ignored
// by the thing it is addressed to; this cannot.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/isrenato/robasko/internal/registry"
)

// Limits from SPEC.md §7.
const (
	MaxDepth = 3
	MaxCalls = 50
)

// Error codes recorded against a call and returned to the caller. They are
// strings because they are stored in tool_calls.error_code and read back
// by anything reconstructing a trace.
const (
	CodeForbidden      = "forbidden"
	CodeInvalidInput   = "invalid_input"
	CodeInvalidOutput  = "invalid_output"
	CodeNotFound       = "not_found"
	CodeDepthExceeded  = "depth_exceeded"
	CodeBudgetExceeded = "budget_exceeded"
	CodeCycleDetected  = "cycle_detected"
	CodeTimeout        = "timeout"
	CodeHandlerFailed  = "handler_failed"
)

// Error is a refusal or a failure, carrying the code that explains it.
type Error struct {
	Code string
	Tool string
	Err  error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("%s: %s", e.Tool, e.Code)
	}
	return fmt.Sprintf("%s: %s: %v", e.Tool, e.Code, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// CodeOf returns the error code behind an error, or "" when it is not a
// runtime refusal.
func CodeOf(err error) string {
	var re *Error
	if errors.As(err, &re) {
		return re.Code
	}
	return ""
}

// Agent is the caller. Scopes decide what it may reach; SPEC.md §8 gives
// each role its own set so that scout cannot score and analyst cannot
// write letters.
type Agent struct {
	ID     string
	Name   string
	Scopes []string
}

// Handler runs one tool.
type Handler func(ctx context.Context, in json.RawMessage) (json.RawMessage, error)

// Call is one invocation as it is recorded.
type Call struct {
	ID           string
	TraceID      string
	TaskID       string
	StepID       string
	ParentCallID string
	Depth        int
	AgentID      string
	Tool         string
	Version      int
	Input        json.RawMessage
	Output       json.RawMessage
	Status       string // running | ok | error
	ErrorCode    string
	StartedAt    time.Time
	Duration     time.Duration
}

// Recorder persists the call log. SPEC.md §17 reconstructs a trace from
// it, so a call is written before the handler runs and finished after,
// which is also what makes an interrupted call visible as `running`.
type Recorder interface {
	Begin(ctx context.Context, c *Call) (id string, err error)
	Finish(ctx context.Context, c *Call) error
	// CountForTask returns how many calls a task has already made, for
	// the budget. It is read once per Execute rather than cached, because
	// sibling agents share a task.
	CountForTask(ctx context.Context, taskID string) (int, error)
	// Replay returns a finished call's output for an idempotency key.
	Replay(ctx context.Context, key string) (json.RawMessage, bool, error)
}

// Resolver finds a registered tool.
type Resolver interface {
	Resolve(ctx context.Context, name string, version int) (*registry.Tool, error)
}

// Runtime executes tools.
type Runtime struct {
	tools    Resolver
	handlers map[string]Handler
	log      Recorder
	// now is swappable so duration can be asserted in tests.
	now func() time.Time
}

// New returns a runtime over a registry and a call log.
func New(tools Resolver, log Recorder) *Runtime {
	return &Runtime{
		tools:    tools,
		handlers: map[string]Handler{},
		log:      log,
		now:      time.Now,
	}
}

// Register attaches a handler to a tool name.
func (r *Runtime) Register(name string, h Handler) { r.handlers[name] = h }

// Request is one call to execute.
type Request struct {
	Agent   Agent
	Tool    string
	Version int // 0 means the latest
	Input   json.RawMessage
	// IdempotencyKey, when set, makes a repeat return the stored output
	// instead of running the handler again. Queue work uses step_id
	// (SPEC.md §7), so a retried step cannot act twice.
	IdempotencyKey string
}

// Execute runs one tool call, enforcing every limit on the way in.
//
// The order of the checks is deliberate: the ones that cost nothing come
// first, and nothing is written to the call log until the call is going
// to happen. A refusal on scope should not leave a row implying the tool
// was reached.
func (r *Runtime) Execute(ctx context.Context, req Request) (json.RawMessage, error) {
	state := stateFrom(ctx)

	if req.IdempotencyKey != "" {
		out, found, err := r.log.Replay(ctx, req.IdempotencyKey)
		if err != nil {
			return nil, &Error{Code: CodeHandlerFailed, Tool: req.Tool, Err: err}
		}
		if found {
			return out, nil
		}
	}

	if state.depth >= MaxDepth {
		return nil, &Error{Code: CodeDepthExceeded, Tool: req.Tool,
			Err: fmt.Errorf("depth %d reaches the limit of %d", state.depth, MaxDepth)}
	}

	// The stack is the path to the root, not every call in the task: a
	// tool used twice in sequence is ordinary, the same tool reached from
	// inside itself is a cycle.
	if state.inStack(req.Tool) {
		return nil, &Error{Code: CodeCycleDetected, Tool: req.Tool,
			Err: fmt.Errorf("already on the call stack: %v", state.stack)}
	}

	tool, err := r.tools.Resolve(ctx, req.Tool, req.Version)
	if err != nil {
		code := CodeNotFound
		if !errors.Is(err, registry.ErrNotFound) {
			code = CodeHandlerFailed
		}
		return nil, &Error{Code: code, Tool: req.Tool, Err: err}
	}

	if !tool.Permits(req.Agent.Scopes) {
		return nil, &Error{Code: CodeForbidden, Tool: req.Tool,
			Err: fmt.Errorf("%s holds %v, tool requires %v",
				req.Agent.Name, req.Agent.Scopes, tool.RequiredScopes)}
	}

	if err := tool.ValidateInput(req.Input); err != nil {
		return nil, &Error{Code: CodeInvalidInput, Tool: req.Tool, Err: err}
	}

	if state.taskID != "" {
		spent, err := r.log.CountForTask(ctx, state.taskID)
		if err != nil {
			return nil, &Error{Code: CodeHandlerFailed, Tool: req.Tool, Err: err}
		}
		if spent >= MaxCalls {
			return nil, &Error{Code: CodeBudgetExceeded, Tool: req.Tool,
				Err: fmt.Errorf("task has made %d calls, the limit is %d", spent, MaxCalls)}
		}
	}

	handler, ok := r.handlers[req.Tool]
	if !ok {
		return nil, &Error{Code: CodeNotFound, Tool: req.Tool,
			Err: fmt.Errorf("registered but no handler is attached")}
	}

	call := &Call{
		TraceID: state.traceID, TaskID: state.taskID, StepID: state.stepID,
		ParentCallID: state.parentCallID, Depth: state.depth,
		AgentID: req.Agent.ID, Tool: tool.Name, Version: tool.Version,
		Input: req.Input, Status: "running", StartedAt: r.now(),
	}
	callID, err := r.log.Begin(ctx, call)
	if err != nil {
		return nil, &Error{Code: CodeHandlerFailed, Tool: req.Tool, Err: err}
	}
	call.ID = callID

	// The child context carries this call as the parent and this tool on
	// the stack, so a nested call through call_tool is attributed and
	// bounded without the handler having to cooperate.
	child := state.push(callID, tool.Name)
	runCtx, cancel := context.WithTimeout(withState(ctx, child), tool.Timeout)
	defer cancel()

	out, handlerErr := handler(runCtx, req.Input)
	call.Duration = r.now().Sub(call.StartedAt)

	switch {
	case handlerErr != nil:
		call.Status, call.ErrorCode = "error", CodeHandlerFailed
		if code := CodeOf(handlerErr); code != "" {
			// A nested refusal keeps its own code, so a trace shows the
			// limit that stopped the chain rather than a generic failure.
			call.ErrorCode = code
		} else if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			call.ErrorCode = CodeTimeout
		}
	default:
		if err := tool.ValidateOutput(out); err != nil {
			call.Status, call.ErrorCode = "error", CodeInvalidOutput
			handlerErr = &Error{Code: CodeInvalidOutput, Tool: req.Tool, Err: err}
		} else {
			call.Status, call.Output = "ok", out
		}
	}
	if req.IdempotencyKey != "" {
		call.StepID = cmpOr(call.StepID, req.IdempotencyKey)
	}

	if err := r.log.Finish(ctx, call); err != nil {
		return nil, &Error{Code: CodeHandlerFailed, Tool: req.Tool, Err: err}
	}
	if handlerErr != nil {
		if CodeOf(handlerErr) != "" {
			return nil, handlerErr
		}
		return nil, &Error{Code: call.ErrorCode, Tool: req.Tool, Err: handlerErr}
	}
	return out, nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
