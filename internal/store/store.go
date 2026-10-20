package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/isrenato/robasko/internal/reference"
	"github.com/isrenato/robasko/internal/scoring"
	"github.com/isrenato/robasko/migrations"
)

// Store persists vacancies and the companies behind them.
type Store struct {
	pool *pgxpool.Pool
	norm *Normaliser
}

// Open connects to Postgres and verifies the connection.
func Open(ctx context.Context, dsn string) (*Store, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, fmt.Errorf("store: empty DATABASE_URL")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: configuring the pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: connecting: %w", err)
	}
	return &Store{pool: pool, norm: NewNormaliser(nil)}, nil
}

func (s *Store) Close() { s.pool.Close() }

// migrationLockKey is an arbitrary but fixed identifier for the advisory
// lock that serialises schema changes.
const migrationLockKey int64 = 7_735_001

// Migrate applies every migration not yet recorded, in filename order, and
// returns the names it applied. Each runs in its own transaction together
// with the row that records it, so a failure leaves neither behind.
//
// The whole run is serialised by a session advisory lock. Without it this
// is a check-then-act race, and SPEC.md §8 starts three agent processes at
// once: each would see a migration as unapplied, each would run it, and
// the losers would fail on the primary key of schema_migrations. The lock
// makes the second process wait and then find nothing to do.
func (s *Store) Migrate(ctx context.Context) ([]string, error) {
	// Held on one connection for the duration, so the lock outlives the
	// per-migration transactions it is protecting.
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: acquiring a connection to migrate: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return nil, fmt.Errorf("store: taking the migration lock: %w", err)
	}
	defer func() {
		// Released on its own context: the caller's may already be done.
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, migrationLockKey); err != nil {
			slog.Warn("store: releasing the migration lock", "error", err)
		}
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       text PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return nil, fmt.Errorf("store: creating schema_migrations: %w", err)
	}

	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)

	var applied []string
	for _, name := range names {
		var exists bool
		if err := conn.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)`, name,
		).Scan(&exists); err != nil {
			return applied, fmt.Errorf("store: checking %s: %w", name, err)
		}
		if exists {
			continue
		}

		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			return applied, err
		}

		tx, err := conn.Begin(ctx)
		if err != nil {
			return applied, err
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			tx.Rollback(ctx) //nolint:errcheck
			return applied, fmt.Errorf("store: applying %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, name); err != nil {
			tx.Rollback(ctx) //nolint:errcheck
			return applied, fmt.Errorf("store: recording %s: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return applied, fmt.Errorf("store: committing %s: %w", name, err)
		}
		applied = append(applied, name)
	}
	return applied, nil
}

func (s *Store) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Vacancy is what a source adapter produced, ready to be stored.
type Vacancy struct {
	Source      string
	SourceID    string
	URL         string
	Title       string
	Company     string
	City        string
	Remote      string // onsite | hybrid | remote | unknown
	PublishedAt time.Time
	Text        string
}

// SaveResult counts what one batch did.
type SaveResult struct {
	New       int
	Duplicate int
}

// Save stores the vacancies that are not already held.
//
// A vacancy is recognised by its fingerprint rather than by the board's own
// identifier, so the same post reaching us from two sources is stored once.
func (s *Store) Save(ctx context.Context, vacancies []Vacancy) (SaveResult, error) {
	var out SaveResult
	for _, v := range vacancies {
		stored, err := s.saveOne(ctx, v)
		if err != nil {
			return out, err
		}
		if stored {
			out.New++
		} else {
			out.Duplicate++
		}
	}
	return out, nil
}

func (s *Store) saveOne(ctx context.Context, v Vacancy) (bool, error) {
	var companyID *string
	if normalised := s.norm.Company(v.Company); normalised != "" {
		var id string
		// DO UPDATE rather than DO NOTHING: only an updated row is returned,
		// so this yields the id whether the company was new or already held.
		err := s.pool.QueryRow(ctx, `
			INSERT INTO companies (name, normalised_name, city)
			VALUES ($1, $2, NULLIF($3, ''))
			ON CONFLICT (normalised_name)
			DO UPDATE SET name = companies.name
			RETURNING id`,
			v.Company, normalised, v.City,
		).Scan(&id)
		if err != nil {
			return false, fmt.Errorf("store: upserting company %q: %w", v.Company, err)
		}
		companyID = &id
	}

	remote := v.Remote
	if remote == "" {
		remote = "unknown"
	}

	var published *time.Time
	if !v.PublishedAt.IsZero() {
		published = &v.PublishedAt
	}

	var language *string
	if l := s.norm.DetectLanguage(v.Text); l != "" {
		language = &l
	}

	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO vacancies
			(source, source_id, url, title, company_id, city, remote,
			 language, published_at, raw_text, fingerprint, fingerprint_terms)
		VALUES ($1, $2, NULLIF($3, ''), $4, $5, NULLIF($6, ''), $7,
		        $8, $9, NULLIF($10, ''), $11, $12)
		ON CONFLICT DO NOTHING
		RETURNING id`,
		v.Source, v.SourceID, v.URL, v.Title, companyID, v.City, remote,
		language, published, v.Text,
		s.norm.Fingerprint(v.Title, v.Company, v.City),
		s.norm.Terms().FingerprintVersion(),
	).Scan(&id)

	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // already held, by fingerprint or by source id
	}
	if err != nil {
		return false, fmt.Errorf("store: inserting vacancy %q: %w", v.Title, err)
	}
	return true, nil
}

// Counts reports how much is stored, for the command line.
func (s *Store) Counts(ctx context.Context) (vacancies, companies int, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM vacancies), (SELECT count(*) FROM companies)`,
	).Scan(&vacancies, &companies)
	return
}

// LoadTerms reads the vocabulary from reference_terms and installs it, so
// subsequent normalisation uses what the database holds rather than what
// was compiled in.
func (s *Store) LoadTerms(ctx context.Context) (*reference.Terms, error) {
	rows, err := s.pool.Query(ctx, `SELECT kind, value FROM reference_terms`)
	if err != nil {
		return nil, fmt.Errorf("store: reading reference_terms: %w", err)
	}
	defer rows.Close()

	lists := map[reference.Kind][]string{}
	for rows.Next() {
		var kind, value string
		if err := rows.Scan(&kind, &value); err != nil {
			return nil, err
		}
		lists[reference.Kind(kind)] = append(lists[reference.Kind(kind)], value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(lists) == 0 {
		return nil, fmt.Errorf("store: reference_terms is empty")
	}

	terms := reference.New(lists)
	s.norm = NewNormaliser(terms)
	return terms, nil
}

// StaleFingerprints counts stored vacancies whose fingerprint was computed
// under a different vocabulary than the one now loaded. Anything above zero
// means dedup can no longer recognise those rows, and they need
// re-fingerprinting before the count is trusted.
func (s *Store) StaleFingerprints(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM vacancies
		 WHERE fingerprint_terms IS DISTINCT FROM $1`,
		s.norm.Terms().FingerprintVersion(),
	).Scan(&n)
	return n, err
}

// Pool exposes the connection pool for packages that own their own
// queries, such as the tool registry.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Scored is one vacancy's result, ready to be stored.
type Scored struct {
	VacancyID       string
	Requirements    []scoring.Requirement
	Categories      []string // parallel to Requirements; "" where unknown
	Result          scoring.Result
	Model           string
	InventoryDigest string
}

// SaveScore stores a vacancy's requirement table and its score.
//
// The whole result replaces any previous one in a single transaction:
// re-scoring a vacancy against a changed inventory must not leave half the
// old requirement table behind, which would make the stored score
// unexplainable by the rows meant to explain it.
func (s *Store) SaveScore(ctx context.Context, sc Scored) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`DELETE FROM requirements WHERE vacancy_id = $1`, sc.VacancyID); err != nil {
			return fmt.Errorf("clearing requirements: %w", err)
		}

		for i, r := range sc.Requirements {
			var category *string
			if i < len(sc.Categories) && sc.Categories[i] != "" {
				category = &sc.Categories[i]
			}
			ids := r.IDs
			if ids == nil {
				ids = []string{}
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO requirements
					(vacancy_id, seq, text, kind, category, canonical_ids, coverage)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				sc.VacancyID, i, r.Text, string(r.Kind), category, ids, r.Coverage,
			); err != nil {
				return fmt.Errorf("inserting requirement %d: %w", i, err)
			}
		}

		res := sc.Result
		if _, err := tx.Exec(ctx, `
			INSERT INTO scores
				(vacancy_id, score, must_coverage, nice_coverage, must_count,
				 nice_count, capped_reason, must_gaps, nice_gaps, model,
				 inventory_digest, computed_at)
			VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8, $9, NULLIF($10, ''), NULLIF($11, ''), now())
			ON CONFLICT (vacancy_id) DO UPDATE SET
				score = EXCLUDED.score,
				must_coverage = EXCLUDED.must_coverage,
				nice_coverage = EXCLUDED.nice_coverage,
				must_count = EXCLUDED.must_count,
				nice_count = EXCLUDED.nice_count,
				capped_reason = EXCLUDED.capped_reason,
				must_gaps = EXCLUDED.must_gaps,
				nice_gaps = EXCLUDED.nice_gaps,
				model = EXCLUDED.model,
				inventory_digest = EXCLUDED.inventory_digest,
				computed_at = now()`,
			sc.VacancyID, res.Score, res.MustCoverage, res.NiceCoverage,
			res.MustCount, res.NiceCount, res.CappedReason,
			nonNil(res.MustGaps), nonNil(res.NiceGaps), sc.Model, sc.InventoryDigest,
		); err != nil {
			return fmt.Errorf("storing the score: %w", err)
		}

		// A scored vacancy leaves the review queue's "new" bucket.
		_, err := tx.Exec(ctx,
			`UPDATE vacancies SET status = 'scored' WHERE id = $1 AND status = 'new'`,
			sc.VacancyID)
		return err
	})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Unscored returns vacancies that have no score yet, newest first.
func (s *Store) Unscored(ctx context.Context, limit int) ([]StoredVacancy, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.pool.Query(ctx, `
		SELECT v.id, v.title, coalesce(c.name, ''), coalesce(v.city, ''),
		       v.remote, coalesce(v.raw_text, ''), coalesce(v.url, '')
		FROM vacancies v
		LEFT JOIN companies c ON c.id = v.company_id
		LEFT JOIN scores s ON s.vacancy_id = v.id
		WHERE s.vacancy_id IS NULL AND v.raw_text IS NOT NULL
		ORDER BY v.created_at DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: listing unscored vacancies: %w", err)
	}
	defer rows.Close()

	var out []StoredVacancy
	for rows.Next() {
		var v StoredVacancy
		if err := rows.Scan(&v.ID, &v.Title, &v.Company, &v.City, &v.Remote, &v.Text, &v.URL); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// StoredVacancy is a vacancy read back for scoring.
type StoredVacancy struct {
	ID      string
	Title   string
	Company string
	City    string
	Remote  string
	Text    string
	URL     string
}

// EnsureAgent registers a role and returns its id, so calls can be
// attributed to it in the call log.
//
// The token hash is generated once and left alone afterwards. No endpoint
// authenticates against it yet; the column exists because SPEC.md §5 has
// it, and writing a random value now is better than a NULL that later
// turns into a nullable column nobody tightens.
func (s *Store) EnsureAgent(ctx context.Context, name string, scopes []string) (string, error) {
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return "", fmt.Errorf("store: generating an agent token: %w", err)
	}

	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO agents (name, scopes, token_hash)
		VALUES ($1, $2, $3)
		ON CONFLICT (name) DO UPDATE SET scopes = EXCLUDED.scopes
		RETURNING id`, name, scopes, hex.EncodeToString(token)).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store: registering agent %q: %w", name, err)
	}
	return id, nil
}
