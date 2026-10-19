package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func get(t *testing.T, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// Liveness must not depend on the database: if it did, a database outage
// would make the orchestrator restart healthy processes.
func TestHealthzIgnoresTheDatabase(t *testing.T) {
	mux := newMux("scout", fakePinger{err: errors.New("connection refused")})

	rec := get(t, mux, "/healthz")
	if rec.Code != http.StatusOK {
		t.Errorf("got %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "ok" {
		t.Errorf("got %q, want %q", got, "ok")
	}
}

func TestReadyzReportsReadyWhenTheDatabaseAnswers(t *testing.T) {
	mux := newMux("analyst", fakePinger{})

	rec := get(t, mux, "/readyz")
	if rec.Code != http.StatusOK {
		t.Errorf("got %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "ready" {
		t.Errorf("got %q, want %q", got, "ready")
	}
}

func TestReadyzReportsUnavailableWhenThePingFails(t *testing.T) {
	mux := newMux("analyst", fakePinger{err: errors.New("connection refused")})

	rec := get(t, mux, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d, want 503", rec.Code)
	}
}

// A missing DATABASE_URL is a configuration error, not a mode to serve
// through, so the process stays permanently unready rather than pretending.
func TestReadyzReportsUnavailableWithoutADatabase(t *testing.T) {
	mux := newMux("writer", nil)

	rec := get(t, mux, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no database") {
		t.Errorf("response does not say why: %q", rec.Body.String())
	}
}

func TestUnknownRoleIsRejected(t *testing.T) {
	err := run("courier", ":0")
	if err == nil {
		t.Fatal("expected an unknown role to be rejected")
	}
	if !strings.Contains(err.Error(), "courier") {
		t.Errorf("error does not name the offending role: %v", err)
	}
}

func TestEveryDocumentedRoleIsAccepted(t *testing.T) {
	for _, role := range []string{"scout", "analyst", "writer"} {
		if !roles[role] {
			t.Errorf("role %q from SPEC.md 8 is not accepted", role)
		}
	}
}
