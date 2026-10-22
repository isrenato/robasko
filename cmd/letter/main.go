// Command letter drafts a cover letter for a scored vacancy.
//
// It is the writer's work run by hand. The tool it calls is the same
// draft_letter the writer role claims from the queue, reached through the
// same runtime, so a letter written here is written under the same scope
// check, the same schema validation and the same call log as one written
// by the daemon. A command that bypassed the runtime would be a second
// implementation of the rule that matters most here, and the second one
// is always the one that is wrong.
//
// Nothing is sent. The result is a .txt and a .pdf under letters/ and a
// row with status `draft`; approving it is a human's act (SPEC.md §13.6).
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/isrenato/robasko/internal/config"
	"github.com/isrenato/robasko/internal/inventory"
	"github.com/isrenato/robasko/internal/letters"
	"github.com/isrenato/robasko/internal/llm"
	"github.com/isrenato/robasko/internal/orchestrator"
	"github.com/isrenato/robasko/internal/paths"
	"github.com/isrenato/robasko/internal/profile"
	"github.com/isrenato/robasko/internal/queue"
	"github.com/isrenato/robasko/internal/registry"
	"github.com/isrenato/robasko/internal/runtime"
	"github.com/isrenato/robasko/internal/store"
	"github.com/isrenato/robasko/internal/style"
	"github.com/isrenato/robasko/internal/tools"
)

type options struct {
	envFile   string
	invPath   string
	profPath  string
	stylePath string
	vacancy   string
	limit     int
	enqueue   bool

	contactFirst  string
	contactLast   string
	contactGender string
}

func main() {
	var o options
	flag.StringVar(&o.envFile, "env", "env.lock", "file holding the settings")
	flag.StringVar(&o.invPath, "inventory", "inventory.yaml", "skills inventory")
	flag.StringVar(&o.profPath, "profile", "profile.yaml", "citable facts")
	flag.StringVar(&o.stylePath, "style", "style.yaml", "how the letter is written")
	flag.StringVar(&o.vacancy, "vacancy", "", "a vacancy id; the best candidates are used when empty")
	flag.IntVar(&o.limit, "limit", 1, "how many letters to draft when no vacancy is named")
	flag.BoolVar(&o.enqueue, "enqueue", false,
		"hand the work to the writer role through the queue instead of doing it here")
	flag.StringVar(&o.contactFirst, "contact-first", "", "the contact's first name, where the advertisement gives one")
	flag.StringVar(&o.contactLast, "contact-last", "", "the contact's surname")
	flag.StringVar(&o.contactGender, "contact-gender", "",
		`"f" or "m"; left empty the salutation does not guess one`)
	flag.Parse()

	if err := run(o); err != nil {
		fmt.Fprintf(os.Stderr, "letter: %v\n", err)
		os.Exit(1)
	}
}

func run(o options) error {
	if o.contactGender != "" && o.contactGender != "f" && o.contactGender != "m" {
		return fmt.Errorf(`-contact-gender must be "f", "m" or empty, not %q`, o.contactGender)
	}

	look, err := config.LoadFile(o.envFile)
	if err != nil {
		return err
	}
	dsn := look("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	st, err := store.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer st.Close()

	if _, err := st.Migrate(ctx); err != nil {
		return fmt.Errorf("applying migrations: %w", err)
	}

	subjects, err := pick(ctx, st, o)
	if err != nil {
		return err
	}
	if len(subjects) == 0 {
		fmt.Printf("nothing to write: no scored vacancy is at or above %.0f without a letter\n",
			letters.Threshold)
		return nil
	}

	q := queue.NewPostgres(st.Pool())
	if o.enqueue {
		return handOver(ctx, q, subjects, o)
	}

	deps, err := load(o)
	if err != nil {
		return err
	}
	deps.Store = st

	rt := runtime.New(registry.New(st.Pool()), st.Calls())
	installed, err := tools.Install(ctx, registry.New(st.Pool()), rt, deps, []string{tools.DraftLetter})
	if err != nil {
		return err
	}
	if len(installed) == 0 {
		return fmt.Errorf("draft_letter did not install")
	}

	// The writer's own scopes, not a wider set. Running by hand is not a
	// reason to hold permissions the role would not have.
	role := orchestrator.Roles["writer"]
	agentID, err := st.EnsureAgent(ctx, role.Name, role.Scopes)
	if err != nil {
		return err
	}
	agent := runtime.Agent{ID: agentID, Name: role.Name, Scopes: role.Scopes}

	for _, id := range subjects {
		if err := draft(ctx, st, q, rt, agent, id, o); err != nil {
			// One vacancy failing is not a reason to abandon the rest.
			fmt.Printf("   FAILED: %v\n\n", err)
		}
	}
	return nil
}

// pick resolves which vacancies to write for.
func pick(ctx context.Context, st *store.Store, o options) ([]string, error) {
	if o.vacancy != "" {
		return []string{o.vacancy}, nil
	}
	return st.LetterCandidates(ctx, letters.Threshold, o.limit)
}

// load reads the three hand-maintained files and opens the model.
func load(o options) (tools.Deps, error) {
	var d tools.Deps
	var err error

	if d.Inventory, err = inventory.Load(o.invPath); err != nil {
		return d, err
	}
	if d.Profile, err = profile.Load(o.profPath); err != nil {
		return d, err
	}
	if d.Style, err = style.Load(o.stylePath); err != nil {
		return d, err
	}
	for _, warning := range d.Style.Warnings() {
		fmt.Fprintf(os.Stderr, "warning: %s\n", warning)
	}

	look, err := config.LoadFile(o.envFile)
	if err != nil {
		return d, err
	}
	if d.Model, err = llm.New(llm.Options{
		APIKey:      look("ANTHROPIC_API_KEY"),
		BaseURL:     look("ANTHROPIC_BASE_URL"),
		Model:       look("ANTHROPIC_MODEL"),
		WorkspaceID: look("ANTHROPIC_WORKSPACE_ID"),
	}); err != nil {
		return d, err
	}

	root, err := os.Getwd()
	if err != nil {
		return d, err
	}
	d.RepoRoot = root
	if d.LettersDir, err = paths.Letters(root); err != nil {
		return d, err
	}

	script := filepath.Join(root, "scripts", "build_letter.py")
	if _, err := os.Stat(script); err == nil {
		d.Script = script
	} else {
		fmt.Fprintf(os.Stderr, "warning: no renderer at %s; the draft will be .txt only\n", script)
	}

	d.Log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return d, nil
}

// draft runs one letter through the runtime and prints what came of it.
func draft(ctx context.Context, st *store.Store, q *queue.Postgres, rt *runtime.Runtime,
	agent runtime.Agent, vacancyID string, o options) error {

	sub, err := st.LetterSubject(ctx, vacancyID)
	if err != nil {
		return err
	}
	fmt.Printf("%s\n   %s | %s | score %.1f\n", sub.Title, sub.Company, sub.City, sub.Result.Score)

	in, err := json.Marshal(map[string]string{
		"vacancy_id":         vacancyID,
		"contact_first_name": o.contactFirst,
		"contact_last_name":  o.contactLast,
		"contact_gender":     o.contactGender,
	})
	if err != nil {
		return err
	}

	// A task and a trace, so the call lands in the log where a trace can
	// be reconstructed from it rather than floating free.
	taskID, err := q.CreateTask(ctx, traceID(), "draft a cover letter", vacancyID, agent.ID)
	if err != nil {
		return err
	}

	out, err := rt.Execute(runtime.Context(ctx, traceID(), taskID, ""), runtime.Request{
		Agent: agent, Tool: tools.DraftLetter, Input: in,
	})
	if err != nil {
		return err
	}

	var result struct {
		Status    string   `json:"status"`
		TxtPath   string   `json:"txt_path"`
		PDFPath   string   `json:"pdf_path"`
		FactsUsed []string `json:"facts_used"`
		Words     int      `json:"words"`
		Cuts      []string `json:"cuts"`
		Reason    string   `json:"reason"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return err
	}

	fmt.Printf("   %s, %d words, %d sources cited\n", result.Status, result.Words, len(result.FactsUsed))
	fmt.Printf("   %s\n", result.TxtPath)
	if result.PDFPath != "" {
		fmt.Printf("   %s\n", result.PDFPath)
	}
	for _, cut := range result.Cuts {
		fmt.Printf("   shortened: cut %s to fit one page\n", cut)
	}
	if result.Reason != "" {
		fmt.Printf("   %s\n", result.Reason)
	}
	fmt.Println("   read it before anyone else does; nothing is sent from here")
	fmt.Println()
	return nil
}

// handOver queues the work for the writer role instead of doing it here.
func handOver(ctx context.Context, q *queue.Postgres, subjects []string, o options) error {
	for _, id := range subjects {
		in, err := json.Marshal(map[string]string{
			"vacancy_id":         id,
			"contact_first_name": o.contactFirst,
			"contact_last_name":  o.contactLast,
			"contact_gender":     o.contactGender,
		})
		if err != nil {
			return err
		}
		taskID, err := q.CreateTask(ctx, traceID(), "draft a cover letter", id, "")
		if err != nil {
			return err
		}
		step, err := q.Enqueue(ctx, taskID, tools.DraftLetter, in)
		if err != nil {
			return err
		}
		fmt.Printf("queued %s for the writer: task %s step %s\n", id, taskID, step.ID)
	}
	return nil
}

// traceID is a random identifier for one chain of calls.
func traceID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// Only the readability of a log line depends on this.
		return fmt.Sprintf("trace-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
