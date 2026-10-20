// Command agentd runs one agent role.
//
// SPEC.md §8 gives each role its own process, token and scope set, so that
// the scope check has something to enforce: scout cannot score, analyst
// cannot write letters. The roles differ only in configuration; this is one
// binary started three ways.
//
// A role that claims steps works the queue for as long as the process
// lives: claim, run the tool through the runtime, record the outcome.
// scout has no steps to claim - saved searches drive it - so it serves
// health only.
//
// What is not here yet is the domain tools themselves. The registry, the
// runtime and the queue are built, but nothing registers sources.fetch,
// score.compute or letter.draft, so a queued step resolves to a tool that
// does not exist.
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

	"github.com/isrenato/robasko/internal/orchestrator"
	"github.com/isrenato/robasko/internal/paths"
	"github.com/isrenato/robasko/internal/queue"
	"github.com/isrenato/robasko/internal/registry"
	"github.com/isrenato/robasko/internal/runtime"
	"github.com/isrenato/robasko/internal/store"
)

// pollInterval is how long a role waits when the queue is empty. Short
// enough that a handover is picked up promptly, long enough that three
// idle processes are not hammering one table.
const pollInterval = 2 * time.Second

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
	roleDef, err := orchestrator.Lookup(role)
	if err != nil {
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
		st, err := store.Open(ctx, dsn)
		if err != nil {
			return err
		}
		defer st.Close()

		// The schema is applied here rather than by a separate command, so
		// that bringing the stack up is one step and a fresh database is
		// never served against. Concurrent roles are safe: Migrate takes an
		// advisory lock, and the ones that lose the race find nothing to do.
		applied, err := st.Migrate(ctx)
		if err != nil {
			return fmt.Errorf("applying migrations: %w", err)
		}
		for _, name := range applied {
			log.Info("migration applied", "name", name)
		}
		if len(applied) == 0 {
			log.Info("schema up to date")
		}

		db = st.Pool()

		// A role that claims steps works the queue for as long as the
		// process lives. scout has no steps to claim - it is driven by
		// saved searches - so it serves health only.
		if len(roleDef.Handles) > 0 {
			agentID, err := st.EnsureAgent(ctx, roleDef.Name, roleDef.Scopes)
			if err != nil {
				return err
			}

			rt := runtime.New(registry.New(st.Pool()), st.Calls())
			worker := orchestrator.New(roleDef, agentID, queue.NewPostgres(st.Pool()), rt, log)

			go func() {
				if err := worker.Run(ctx, pollInterval); err != nil &&
					!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					log.Error("the worker stopped", "error", err)
				}
			}()
		}
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
