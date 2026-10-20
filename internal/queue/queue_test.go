package queue

import (
	"testing"
	"time"
)

// SPEC.md §8: a failure waits 2^attempts x 5s before the next try, so a
// transient fault comes back soon and a persistent one stops occupying
// the queue.
func TestBackoffGrows(t *testing.T) {
	want := map[int]time.Duration{
		0: 5 * time.Second,
		1: 10 * time.Second,
		2: 20 * time.Second,
		3: 40 * time.Second,
	}
	for attempts, d := range want {
		if got := Backoff(attempts); got != d {
			t.Errorf("Backoff(%d) = %v, want %v", attempts, got, d)
		}
	}
	// A negative count is a bug upstream, not a reason to panic or to
	// schedule something in the past.
	if got := Backoff(-1); got != BackoffBase {
		t.Errorf("Backoff(-1) = %v, want %v", got, BackoffBase)
	}
}

// The envelope is what a trace shows (SPEC.md §8), so it has to carry
// enough to reconstruct the handover without reading the database.
func TestHandoverCarriesTheWholeChain(t *testing.T) {
	s := &Step{
		ID: "step-1", TaskID: "task-1", TraceID: "trace-1",
		Seq: 2, ToolName: "letter.draft", Attempts: 1,
	}
	deadline := time.Date(2026, 10, 20, 18, 0, 0, 0, time.UTC)

	e := Handover(s, "analyst", "writer", deadline)
	if e.TraceID != "trace-1" || e.TaskID != "task-1" || e.StepID != "step-1" {
		t.Errorf("identity missing: %+v", e)
	}
	if e.From != "analyst" || e.To != "writer" {
		t.Errorf("the handover does not say who passed to whom: %+v", e)
	}
	if e.Tool != "letter.draft" || e.Seq != 2 || e.Attempt != 1 {
		t.Errorf("step details missing: %+v", e)
	}
	if !e.Deadline.Equal(deadline) {
		t.Errorf("deadline %v", e.Deadline)
	}
}
