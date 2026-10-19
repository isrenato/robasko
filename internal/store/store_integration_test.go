package store

import (
	"context"
	"io/fs"
	"os"
	"sync"
	"testing"
	"time"

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
