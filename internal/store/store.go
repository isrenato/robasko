package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/isrenato/robasko/internal/reference"
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

// Migrate applies every migration not yet recorded, in filename order, and
// returns the names it applied. Each runs in its own transaction together
// with the row that records it, so a failure leaves neither behind.
func (s *Store) Migrate(ctx context.Context) ([]string, error) {
	_, err := s.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       text PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`)
	if err != nil {
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
		if err := s.pool.QueryRow(ctx,
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
		if err := s.inTx(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, name)
			return err
		}); err != nil {
			return applied, fmt.Errorf("store: applying %s: %w", name, err)
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
