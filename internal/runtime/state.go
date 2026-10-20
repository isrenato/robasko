package runtime

import "context"

// state travels with a call: who it belongs to, how deep it is, and which
// tools are already on the path to the root.
//
// It lives in the context rather than in a parameter because a nested
// call goes through a handler the runtime does not control. A handler
// that forwards its context - which it must, to be cancellable - carries
// the depth, the parent and the stack with it whether or not it knows
// they exist. A handler cannot opt out of the limits by forgetting to
// pass something along.
type state struct {
	traceID      string
	taskID       string
	stepID       string
	parentCallID string
	depth        int
	stack        []string
}

type stateKey struct{}

// Context returns a context carrying the identity of a top-level call.
// Everything nested below it inherits the trace, the task and the step.
func Context(ctx context.Context, traceID, taskID, stepID string) context.Context {
	return withState(ctx, state{traceID: traceID, taskID: taskID, stepID: stepID})
}

func withState(ctx context.Context, s state) context.Context {
	return context.WithValue(ctx, stateKey{}, s)
}

func stateFrom(ctx context.Context) state {
	if s, ok := ctx.Value(stateKey{}).(state); ok {
		return s
	}
	return state{}
}

// push returns the state a nested call runs under.
func (s state) push(callID, tool string) state {
	next := s
	next.parentCallID = callID
	next.depth = s.depth + 1
	// Copied rather than appended in place: two sibling calls from one
	// parent must not see each other on the stack, and append would share
	// the backing array between them.
	next.stack = make([]string, len(s.stack), len(s.stack)+1)
	copy(next.stack, s.stack)
	next.stack = append(next.stack, tool)
	return next
}

// inStack reports whether a tool is already on the path to the root.
func (s state) inStack(tool string) bool {
	for _, t := range s.stack {
		if t == tool {
			return true
		}
	}
	return false
}

// Depth reports how deep the current call is, for a handler that wants to
// log it.
func Depth(ctx context.Context) int { return stateFrom(ctx).depth }

// TraceID returns the trace the current call belongs to.
func TraceID(ctx context.Context) string { return stateFrom(ctx).traceID }
