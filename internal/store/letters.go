package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/isrenato/robasko/internal/scoring"
	"github.com/jackc/pgx/v5"
)

// ErrNoScore reports a vacancy that has not been scored, or does not
// exist. The two are one error on purpose: the writer may only act on a
// scored vacancy, and from where it stands "not scored yet" and "no such
// vacancy" call for the same thing, which is to stop.
var ErrNoScore = errors.New("the vacancy has no score")

// LetterSubject is everything the writer needs about one vacancy. It is
// read in one place rather than assembled by the caller, because a letter
// written against a score and a requirement table that were read
// separately can be written against two different versions of them.
type LetterSubject struct {
	StoredVacancy
	Language     string
	Result       scoring.Result
	Requirements []scoring.Requirement
}

// LetterSubject reads a scored vacancy and its requirement table.
func (s *Store) LetterSubject(ctx context.Context, vacancyID string) (*LetterSubject, error) {
	var sub LetterSubject
	err := s.pool.QueryRow(ctx, `
		SELECT v.id, v.title, coalesce(c.name, ''), coalesce(v.city, ''),
		       v.remote, coalesce(v.raw_text, ''), coalesce(v.url, ''),
		       coalesce(v.language, ''),
		       sc.score, sc.must_coverage, sc.nice_coverage,
		       sc.must_count, sc.nice_count, coalesce(sc.capped_reason, ''),
		       sc.must_gaps, sc.nice_gaps
		FROM vacancies v
		JOIN scores sc ON sc.vacancy_id = v.id
		LEFT JOIN companies c ON c.id = v.company_id
		WHERE v.id = $1`, vacancyID,
	).Scan(&sub.ID, &sub.Title, &sub.Company, &sub.City, &sub.Remote,
		&sub.Text, &sub.URL, &sub.Language,
		&sub.Result.Score, &sub.Result.MustCoverage, &sub.Result.NiceCoverage,
		&sub.Result.MustCount, &sub.Result.NiceCount, &sub.Result.CappedReason,
		&sub.Result.MustGaps, &sub.Result.NiceGaps)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNoScore, vacancyID)
	}
	if err != nil {
		return nil, fmt.Errorf("store: reading vacancy %s: %w", vacancyID, err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT text, kind, canonical_ids, coverage
		FROM requirements WHERE vacancy_id = $1 ORDER BY seq`, vacancyID)
	if err != nil {
		return nil, fmt.Errorf("store: reading requirements for %s: %w", vacancyID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var r scoring.Requirement
		var kind string
		if err := rows.Scan(&r.Text, &kind, &r.IDs, &r.Coverage); err != nil {
			return nil, err
		}
		r.Kind = scoring.Kind(kind)
		sub.Requirements = append(sub.Requirements, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &sub, nil
}

// LetterCandidates returns the vacancies that warrant a letter and do not
// have one, best first.
//
// The must-gap condition is in the query rather than left to the caller.
// SPEC.md §13 makes it a precondition of drafting at all, and a condition
// enforced only in the code that happens to call this is a condition that
// is not enforced.
func (s *Store) LetterCandidates(ctx context.Context, threshold float64, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.pool.Query(ctx, `
		SELECT sc.vacancy_id
		FROM scores sc
		LEFT JOIN letters l ON l.vacancy_id = sc.vacancy_id
		WHERE sc.score >= $1
		  AND cardinality(sc.must_gaps) = 0
		  AND l.vacancy_id IS NULL
		ORDER BY sc.score DESC
		LIMIT $2`, threshold, limit)
	if err != nil {
		return nil, fmt.Errorf("store: listing letter candidates: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Letter is a drafted letter as it is stored. The body is not here: it is
// the file at TxtPath, and one copy of a biography is enough.
type Letter struct {
	ID        string
	VacancyID string
	TxtPath   string
	PDFPath   string
	FactsUsed []string
	Status    string
	Reason    string
	Model     string
}

// SaveLetter stores a draft and moves its vacancy on.
//
// Redrafting replaces the row, for the same reason the file on disk is
// overwritten: two drafts for one vacancy is two letters that could be
// sent, and only one of them is the current one.
func (s *Store) SaveLetter(ctx context.Context, l Letter) (string, error) {
	var id string
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO letters (vacancy_id, txt_path, pdf_path, facts_used,
			                     status, reason, model)
			VALUES ($1, $2, NULLIF($3, ''), $4, $5, NULLIF($6, ''), NULLIF($7, ''))
			ON CONFLICT (vacancy_id) DO UPDATE SET
				txt_path = EXCLUDED.txt_path,
				pdf_path = EXCLUDED.pdf_path,
				facts_used = EXCLUDED.facts_used,
				status = EXCLUDED.status,
				reason = EXCLUDED.reason,
				model = EXCLUDED.model,
				created_at = now()
			RETURNING id`,
			l.VacancyID, l.TxtPath, l.PDFPath, nonNil(l.FactsUsed),
			l.Status, l.Reason, l.Model,
		).Scan(&id); err != nil {
			return fmt.Errorf("store: saving the letter: %w", err)
		}

		// Only from `scored`. A vacancy a human has since rejected or
		// archived does not come back because a draft was written for it.
		_, err := tx.Exec(ctx, `
			UPDATE vacancies SET status = 'letter_drafted'
			WHERE id = $1 AND status = 'scored'`, l.VacancyID)
		return err
	})
	return id, err
}
