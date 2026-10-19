package store

import (
	"context"
	"io/fs"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/isrenato/robasko/internal/scoring"
	"github.com/isrenato/robasko/migrations"
)

// openTest connects to the database named by ROBASKO_TEST_DATABASE_URL and
// skips when it is unset, so `go test ./...` stays runnable with nothing
// but a Go toolchain.
func openTest(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("ROBASKO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ROBASKO_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return s
}

func TestMigrateIsIdempotent(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	applied, err := s.Migrate(ctx)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("re-applied %v on an up-to-date schema", applied)
	}
}

func TestLoadTermsMatchesTheCompiledDefault(t *testing.T) {
	s := openTest(t)

	terms, err := s.LoadTerms(context.Background())
	if err != nil {
		t.Fatalf("LoadTerms: %v", err)
	}
	// If the seed and the compiled default agree, so must the version that
	// stored fingerprints are recorded against.
	if got, want := terms.FingerprintVersion(), NewNormaliser(nil).Terms().FingerprintVersion(); got != want {
		t.Errorf("database vocabulary %s differs from the compiled one %s", got, want)
	}
}

// The same post arriving twice, and the same post arriving from a second
// board under a different identifier, are both one vacancy.
func TestSaveDeduplicatesByFingerprint(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	unique := time.Now().UnixNano()
	base := Vacancy{
		Source: "board-a", SourceID: "a-1", Title: "Backend Engineer",
		Company: "Testfirma Nordwind GmbH", City: "Teststadt",
		Remote: "remote", Text: "We are looking for you with experience",
	}
	base.Title = base.Title + " " + time.Unix(0, unique).Format("150405.000000000")

	first, err := s.Save(ctx, []Vacancy{base})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if first.New != 1 {
		t.Fatalf("got %+v, want one new", first)
	}

	same, _ := s.Save(ctx, []Vacancy{base})
	if same.New != 0 || same.Duplicate != 1 {
		t.Errorf("the same vacancy was stored twice: %+v", same)
	}

	// Another board, another identifier, the company written without its
	// legal form: still the same post.
	crossBoard := base
	crossBoard.Source, crossBoard.SourceID = "board-b", "b-9"
	crossBoard.Company = "Testfirma Nordwind"
	other, _ := s.Save(ctx, []Vacancy{crossBoard})
	if other.New != 0 {
		t.Errorf("the same post from a second board was stored again: %+v", other)
	}
}

func TestSaveRecordsTheFingerprintVocabulary(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	if _, err := s.LoadTerms(ctx); err != nil {
		t.Fatalf("LoadTerms: %v", err)
	}
	stale, err := s.StaleFingerprints(ctx)
	if err != nil {
		t.Fatalf("StaleFingerprints: %v", err)
	}
	if stale != 0 {
		t.Errorf("%d rows were fingerprinted under a different vocabulary", stale)
	}
}

// SPEC.md §8 starts three agent processes at once and each applies the
// schema on startup. Without the advisory lock this is a check-then-act
// race: every process sees a migration as unapplied, every one runs it,
// and the losers fail on the primary key of schema_migrations.
func TestConcurrentMigrationsDoNotRace(t *testing.T) {
	dsn := os.Getenv("ROBASKO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ROBASKO_TEST_DATABASE_URL is not set")
	}

	const racers = 4
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		errs    []error
		applied int
	)

	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			s, err := Open(ctx, dsn)
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			defer s.Close()

			<-start // start together, to make the race likely
			names, err := s.Migrate(ctx)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			}
			applied += len(names)
		}()
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		t.Errorf("a concurrent migration failed: %v", err)
	}

	// The count is what proves the lock. On an up-to-date schema nobody
	// applies anything; on a fresh one the migrations are applied exactly
	// once in total, spread across whichever processes won. Anything above
	// that means two processes applied the same migration, which is the
	// failure this guards against.
	files, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatalf("listing migrations: %v", err)
	}
	if applied != 0 && applied != len(files) {
		t.Errorf("%d migrations applied across %d processes; want 0 (already migrated) or %d (fresh), and never more",
			applied, racers, len(files))
	}
}

// Re-scoring replaces the whole result. A requirement table left half
// rewritten would no longer explain the score stored beside it, which is
// the only reason to keep the table at all.
func TestSaveScoreReplacesThePreviousResult(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	v := Vacancy{
		Source: "board-score", SourceID: "s-" + time.Now().Format("150405.000000000"),
		Title:   "Scored " + time.Now().Format("150405.000000000"),
		Company: "Testfirma Scoring GmbH", City: "Teststadt", Remote: "remote",
		Text: "We are looking for you with experience",
	}
	if _, err := s.Save(ctx, []Vacancy{v}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var id string
	if err := s.pool.QueryRow(ctx,
		`SELECT id FROM vacancies WHERE source = $1 AND source_id = $2`,
		v.Source, v.SourceID).Scan(&id); err != nil {
		t.Fatalf("finding the vacancy: %v", err)
	}

	first := Scored{
		VacancyID: id,
		Requirements: []scoring.Requirement{
			{Text: "Go", Kind: scoring.Must, IDs: []string{"go"}, Coverage: 1},
			{Text: "Kafka", Kind: scoring.Must, Coverage: 0},
			{Text: "Nice one", Kind: scoring.Nice, Coverage: 0.8},
		},
		Categories:      []string{"language", "framework", "practice"},
		Result:          scoring.Result{Score: 70, MustCoverage: 0.5, MustCount: 2, NiceCount: 1, MustGaps: []string{"Kafka"}, CappedReason: "1 must-have(s) not covered"},
		Model:           "test-model",
		InventoryDigest: "digest-one",
	}
	if err := s.SaveScore(ctx, first); err != nil {
		t.Fatalf("SaveScore: %v", err)
	}

	assertCounts := func(wantReqs int, wantScore float64, wantDigest string) {
		t.Helper()
		var n int
		if err := s.pool.QueryRow(ctx,
			`SELECT count(*) FROM requirements WHERE vacancy_id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("counting requirements: %v", err)
		}
		if n != wantReqs {
			t.Errorf("got %d requirements, want %d", n, wantReqs)
		}
		var score float64
		var digest string
		if err := s.pool.QueryRow(ctx,
			`SELECT score, coalesce(inventory_digest,'') FROM scores WHERE vacancy_id = $1`, id,
		).Scan(&score, &digest); err != nil {
			t.Fatalf("reading the score: %v", err)
		}
		if score != wantScore || digest != wantDigest {
			t.Errorf("got score %.1f digest %q, want %.1f %q", score, digest, wantScore, wantDigest)
		}
	}
	assertCounts(3, 70, "digest-one")

	// Scoring again against a changed inventory: fewer requirements, a
	// different number, and nothing left over from the first run.
	second := first
	second.Requirements = first.Requirements[:1]
	second.Categories = first.Categories[:1]
	second.Result = scoring.Result{Score: 100, MustCoverage: 1, MustCount: 1}
	second.InventoryDigest = "digest-two"
	if err := s.SaveScore(ctx, second); err != nil {
		t.Fatalf("second SaveScore: %v", err)
	}
	assertCounts(1, 100, "digest-two")

	// A scored vacancy leaves the "new" bucket.
	var status string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM vacancies WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("reading status: %v", err)
	}
	if status != "scored" {
		t.Errorf("status is %q, want scored", status)
	}

	// And it drops out of the unscored queue.
	pending, err := s.Unscored(ctx, 100)
	if err != nil {
		t.Fatalf("Unscored: %v", err)
	}
	for _, p := range pending {
		if p.ID == id {
			t.Error("a scored vacancy is still listed as unscored")
		}
	}
}
