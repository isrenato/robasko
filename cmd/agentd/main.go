// Command agentd runs one agent role.
//
// SPEC.md §8 gives each role its own process, token and scope set, so that
// the scope check has something to enforce: scout cannot score, analyst
// cannot write letters. The roles differ only in configuration; this is one
// binary started three ways.
//
// Only the health endpoints are implemented so far. The registry, the
// runtime and the queue are specified and not yet built.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/isrenato/robasko/internal/paths"
)

// roles are the agent roles from SPEC.md §8.
var roles = map[string]bool{"scout": true, "analyst": true, "writer": true}

// pinger is the part of the database pool the readiness check needs. Taking
// an interface rather than *pgxpool.Pool keeps the handler testable without
// a database.
type pinger interface {
	Ping(context.Context) error
}

func main() {
	var (
		role        = flag.String("role", "", "agent role: scout, analyst or writer")
		addr        = flag.String("addr", ":8080", "address to listen on")
		healthcheck = flag.Bool("healthcheck", false, "probe this process's own /healthz and exit")
	)
	flag.Parse()

	// The runtime image has no shell and no curl, so the container
	// healthcheck re-executes this binary instead.
	if *healthcheck {
		if err := probe(*addr); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if err := run(*role, *addr); err != nil {
		slog.Error("agentd stopped", "error", err)
		os.Exit(1)
	}
}

func run(role, addr string) error {
	if !roles[role] {
		return fmt.Errorf("unknown role %q: expected scout, analyst or writer", role)
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("role", role)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The data root is resolved at startup rather than on first use, so a
	// root misconfigured to sit inside the repository fails here instead of
	// halfway through a run.
	dataRoot, err := paths.EnsureDataRoot(repoRoot())
	if err != nil {
		return fmt.Errorf("resolving the data root: %w", err)
	}

	var db pinger
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			return fmt.Errorf("configuring the database pool: %w", err)
		}
		defer pool.Close()
		db = pool
	} else {
		log.Warn("DATABASE_URL is not set; readiness will report unavailable")
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           newMux(role, db),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errs := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr, "data_root", dataRoot)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// newMux builds the HTTP surface. db may be nil, in which case the service
// is never ready: a missing database is a configuration error, not a
// degraded mode to serve through.
func newMux(role string, db pinger) *http.ServeMux {
	mux := http.NewServeMux()

	// Liveness: the process is up. It deliberately does not touch the
	// database, so a database outage restarts nothing.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeText(w, http.StatusOK, "ok")
	})

	// Readiness: the process can do its job, which means the database
	// answers.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			writeText(w, http.StatusServiceUnavailable, "no database configured")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			slog.Warn("readiness probe failed", "error", err)
			writeText(w, http.StatusServiceUnavailable, "database unavailable")
			return
		}
		writeText(w, http.StatusOK, "ready")
	})

	return mux
}

func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintln(w, body)
}

// probe performs the container healthcheck against this process.
func probe(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("parsing %q: %w", addr, err)
	}
	if host == "" {
		host = "127.0.0.1"
	}

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %s", resp.Status)
	}
	return nil
}

// repoRoot reports the working directory, which is the repository root when
// the binary runs from a checkout and the container's working directory
// otherwise. It only matters as the location the data root must not be
// inside of.
func repoRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}
