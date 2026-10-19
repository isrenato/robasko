// Package sources fetches vacancies from job boards.
//
// One adapter per response shape, behind a single interface, so the rest of
// the system never learns which board a vacancy came from beyond its name.
package sources

import (
	"context"
	"time"
)

// RawVacancy is a vacancy as the board returned it, before normalisation,
// fingerprinting and storage. Fields a board does not provide stay empty;
// the normaliser decides what to do about that, not the adapter.
type RawVacancy struct {
	SourceID    string // the board's own identifier, unique within a source
	URL         string
	Title       string
	Company     string
	City        string
	Remote      bool
	Tags        []string
	PublishedAt time.Time
	Text        string // the description, stored verbatim for audit
}

// Query is a saved search.
type Query struct {
	What  string
	Where string
	Limit int
}

// Source fetches vacancies for one query.
type Source interface {
	// Name is the identifier stored against every vacancy it returns.
	Name() string
	Fetch(ctx context.Context, q Query) ([]RawVacancy, error)
}
