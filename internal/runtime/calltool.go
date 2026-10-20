package runtime

import (
	"context"
	"encoding/json"
	"fmt"
)

// CallToolName is the built-in tool that lets a tool call another tool.
const CallToolName = "call_tool"

// CallToolSchema is its input shape. It is registered like any other tool
// so that the same policy check applies to it.
func CallToolSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"tool", "input"},
		"properties": map[string]any{
			"tool":    map[string]any{"type": "string", "minLength": 1},
			"version": map[string]any{"type": "integer", "minimum": 0},
			"input":   map[string]any{"type": "object"},
		},
	}
}

// CallToolOutputSchema wraps whatever the target returned, so the nesting
// tool has an output contract of its own.
func CallToolOutputSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"output"},
		"properties": map[string]any{
			"output": map[string]any{"type": "object"},
		},
	}
}

type callToolInput struct {
	Tool    string          `json:"tool"`
	Version int             `json:"version"`
	Input   json.RawMessage `json:"input"`
}

// CallTool returns the handler for the nesting tool.
//
// It recurses through Execute rather than reaching for a handler
// directly, so scopes, schema validation, the call log and every limit
// apply to a nested call exactly as they do to a top-level one. A second,
// weaker path into the handlers is how a runtime's guarantees quietly
// stop being guarantees.
//
// The caller's own scopes are used, not a widened set: nesting must not
// be a way to reach a tool the agent could not call directly.
func (r *Runtime) CallTool(caller Agent) Handler {
	return func(ctx context.Context, in json.RawMessage) (json.RawMessage, error) {
		var args callToolInput
		if err := json.Unmarshal(in, &args); err != nil {
			return nil, &Error{Code: CodeInvalidInput, Tool: CallToolName, Err: err}
		}
		if args.Tool == CallToolName {
			// Caught here as well as by the cycle check, because this one
			// names the mistake precisely.
			return nil, &Error{Code: CodeCycleDetected, Tool: CallToolName,
				Err: fmt.Errorf("call_tool cannot call itself")}
		}

		out, err := r.Execute(ctx, Request{
			Agent:   caller,
			Tool:    args.Tool,
			Version: args.Version,
			Input:   args.Input,
		})
		if err != nil {
			return nil, err
		}

		wrapped, err := json.Marshal(map[string]json.RawMessage{"output": out})
		if err != nil {
			return nil, &Error{Code: CodeInvalidOutput, Tool: CallToolName, Err: err}
		}
		return wrapped, nil
	}
}
