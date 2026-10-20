package queue

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func openQueue(t *testing.T) (*Postgres, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("ROBASKO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ROBASKO_TEST_DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return NewPostgres(pool), pool
}

// task opens a task whose steps this test owns, so concurrent runs do not
// claim each other's work.
func task(t *testing.T, q *Postgres) string {
	t.Helper()
	id, err := q.CreateTask(context.Background(),
		"trace-"+time.Now().Format("150405.000000000"), t.Name(), "", "")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	t.Cleanup(func() {
		q.pool.Exec(context.Background(), `DELETE FROM tasks WHERE id = $1`, id)
	})
	return id
}

// claimFrom takes steps until it finds one belonging to this test, so a
// leftover row from elsewhere does not make the test flap.
func claimFrom(t *testing.T, q *Postgres, taskID string) *Step {
	t.Helper()
	for i := 0; i < 20; i++ {
		s, err := q.Claim(context.Background(), "", nil)
		if errors.Is(err, ErrNoWork) {
			return nil
		}
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if s.TaskID == taskID {
			return s
		}
	}
	t.Fatal("could not reach this test's step")
	return nil
}

func TestEnqueueClaimComplete(t *testing.T) {
	q, _ := openQueue(t)
	ctx := context.Background()
	id := task(t, q)

	step, err := q.Enqueue(ctx, id, "score_vacancy", json.RawMessage(`{"vacancy_id":"v1"}`))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if step.Seq != 0 {
		t.Errorf("first step has seq %d, want 0", step.Seq)
	}

	claimed := claimFrom(t, q, id)
	if claimed == nil {
		t.Fatal("the enqueued step was not claimable")
	}
	if claimed.ToolName != "score_vacancy" || claimed.Attempts != 1 {
		t.Errorf("claimed %+v", claimed)
	}
	// The trace comes from the task, so a handover does not have to carry
	// it by hand.
	if claimed.TraceID == "" {
		t.Error("the claimed step has no trace id")
	}

	if err := q.Complete(ctx, claimed.ID, json.RawMessage(`{"score":83}`)); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var status string
	if err := q.pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("reading the task: %v", err)
	}
	if status != "done" {
		t.Errorf("task status %q, want done once nothing is left", status)
	}
}

// SPEC.md §18: two analysts race for one step and exactly one wins. This
// is what FOR UPDATE SKIP LOCKED is in the design for.
func TestExactlyOneAgentWinsAStep(t *testing.T) {
	q, _ := openQueue(t)
	ctx := context.Background()
	id := task(t, q)

	if _, err := q.Enqueue(ctx, id, "score_vacancy", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	const racers = 6
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []string
	)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for n := 0; n < 20; n++ {
				s, err := q.Claim(ctx, "", nil)
				if errors.Is(err, ErrNoWork) {
					return
				}
				if err != nil {
					return
				}
				if s.TaskID == id {
					mu.Lock()
					winners = append(winners, s.ID)
					mu.Unlock()
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(winners) != 1 {
		t.Errorf("%d agents claimed the same step, want exactly 1", len(winners))
	}
}

// A failure returns the step to the queue with backoff until the attempts
// run out, then it is dead and its task is closed with the reason.
func TestRetriesThenDies(t *testing.T) {
	q, _ := openQueue(t)
	ctx := context.Background()
	id := task(t, q)

	if _, err := q.Enqueue(ctx, id, "flaky", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		s := claimFrom(t, q, id)
		if s == nil {
			t.Fatalf("attempt %d: nothing claimable", attempt)
		}
		if s.Attempts != attempt {
			t.Errorf("attempt counted as %d, want %d", s.Attempts, attempt)
		}

		dead, err := q.Fail(ctx, s.ID, "handler_failed", errors.New("boom"))
		if err != nil {
			t.Fatalf("Fail: %v", err)
		}
		if attempt < MaxAttempts && dead {
			t.Fatalf("died on attempt %d of %d", attempt, MaxAttempts)
		}
		if attempt == MaxAttempts && !dead {
			t.Fatalf("still alive after %d attempts", MaxAttempts)
		}

		if !dead {
			// Backoff puts it out of reach until its time comes.
			var status string
			var available time.Time
			if err := q.pool.QueryRow(ctx,
				`SELECT status, available_at FROM task_steps WHERE id = $1`, s.ID,
			).Scan(&status, &available); err != nil {
				t.Fatal(err)
			}
			if status != Pending {
				t.Errorf("status %q after a retryable failure", status)
			}
			if !available.After(time.Now()) {
				t.Error("the step is immediately claimable again; backoff did not apply")
			}
			// Make it claimable for the next round rather than waiting.
			if _, err := q.pool.Exec(ctx,
				`UPDATE task_steps SET available_at = now() WHERE id = $1`, s.ID); err != nil {
				t.Fatal(err)
			}
		}
	}

	var stepStatus, taskStatus, reason string
	if err := q.pool.QueryRow(ctx, `
		SELECT s.status, t.status, coalesce(t.reason, '')
		FROM task_steps s JOIN tasks t ON t.id = s.task_id
		WHERE s.task_id = $1`, id,
	).Scan(&stepStatus, &taskStatus, &reason); err != nil {
		t.Fatal(err)
	}
	if stepStatus != Dead {
		t.Errorf("step status %q, want dead", stepStatus)
	}
	// A dead step must close its task with a reason, or the daily report
	// has nothing to list.
	if taskStatus != "failed" {
		t.Errorf("task status %q, want failed", taskStatus)
	}
	if reason == "" {
		t.Error("the task was failed without a reason")
	}
}

// A step not yet due is not claimable, which is what makes backoff mean
// anything.
func TestStepsBeforeTheirTimeAreNotClaimed(t *testing.T) {
	q, _ := openQueue(t)
	ctx := context.Background()
	id := task(t, q)

	s, err := q.Enqueue(ctx, id, "later", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := q.pool.Exec(ctx,
		`UPDATE task_steps SET available_at = now() + interval '1 hour' WHERE id = $1`, s.ID); err != nil {
		t.Fatal(err)
	}

	if got := claimFrom(t, q, id); got != nil {
		t.Errorf("claimed a step that is not due: %+v", got)
	}
}

// Steps of one task run in order; the sequence is what the claim query
// orders by.
func TestStepsAreClaimedInOrder(t *testing.T) {
	q, _ := openQueue(t)
	ctx := context.Background()
	id := task(t, q)

	for _, tool := range []string{"first", "second", "third"} {
		if _, err := q.Enqueue(ctx, id, tool, json.RawMessage(`{}`)); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	var order []string
	for i := 0; i < 3; i++ {
		s := claimFrom(t, q, id)
		if s == nil {
			t.Fatalf("only %d steps were claimable", i)
		}
		order = append(order, s.ToolName)
		if err := q.Complete(ctx, s.ID, json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"first", "second", "third"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("claimed %v, want %v", order, want)
		}
	}
}

// Completing a step nobody holds is a bug worth hearing about, not a
// silent no-op.
func TestCompletingAnUnclaimedStepFails(t *testing.T) {
	q, _ := openQueue(t)
	ctx := context.Background()
	id := task(t, q)

	s, err := q.Enqueue(ctx, id, "pending-one", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := q.Complete(ctx, s.ID, json.RawMessage(`{}`)); err == nil {
		t.Error("completed a step that was never claimed")
	}
}
