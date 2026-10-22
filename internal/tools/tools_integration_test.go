package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/isrenato/robasko/internal/inventory"
	"github.com/isrenato/robasko/internal/letters"
	"github.com/isrenato/robasko/internal/llm"
	"github.com/isrenato/robasko/internal/profile"
	"github.com/isrenato/robasko/internal/registry"
	"github.com/isrenato/robasko/internal/runtime"
	"github.com/isrenato/robasko/internal/scoring"
	"github.com/isrenato/robasko/internal/store"
	"github.com/isrenato/robasko/internal/style"
)

const invYAML = `
- id: go
  kind: language
  aliases: [go, golang]
  level: production
  evidence: "Serverless functions on Lambda"
- id: postgres
  kind: db
  aliases: [postgres, postgresql]
  level: production
  evidence: "Ran a 5.7 to 13 migration without downtime"
`

const profYAML = `
- id: fact.role
  kind: employment
  text: "Senior Backend Engineer since 2021"
  verbatim: true
`

const styleYAML = `
sender:
  name: Alex Beispiel
  lines: [Alex Beispiel, alex@example.org]
languages:
  de:
    subject: "Bewerbung als {title}"
    salutation:
      gendered_female: "Sehr geehrte Frau {last_name},"
      gendered_male: "Sehr geehrter Herr {last_name},"
      ungendered: "Guten Tag {first_name} {last_name},"
      no_contact: "Liebes {company}-Team,"
    closing: "Mit freundlichen Grüßen"
    date_format: "2. January 2006"
structure:
  - id: opening
    weight: 3
  - id: evidence
    weight: 5
voice_sample: "Short sentences. Then a stop."
`

const draftJSON = `{
 "salutation": "wird ersetzt",
 "paragraphs": [
   {"id":"opening","sentences":[
     {"text":"Sie suchen jemanden für Go im Backend.","sources":["go"]}]},
   {"id":"evidence","sentences":[
     {"text":"Ich schreibe Go für serverlose Funktionen auf Lambda.","sources":["go"]},
     {"text":"Eine Migration von 5.7 auf 13 habe ich ohne Ausfallzeit geleitet.","sources":["postgres"]}]}
 ],
 "closing": "wird auch ersetzt"
}`

func openTest(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("ROBASKO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ROBASKO_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return st
}

// scoredVacancy inserts one vacancy with a score, and removes it again
// when the test ends. The insert is direct rather than through Save
// because the test needs the id and the language, and Save returns
// neither.
func scoredVacancy(t *testing.T, st *store.Store, result scoring.Result) string {
	t.Helper()
	ctx := context.Background()
	stamp := time.Now().UnixNano()

	var companyID string
	if err := st.Pool().QueryRow(ctx, `
		INSERT INTO companies (name, normalised_name, city)
		VALUES ($1, $2, 'Freiburg') RETURNING id`,
		"Nordwind Logistik GmbH", fmt.Sprintf("test-nordwind-%d", stamp),
	).Scan(&companyID); err != nil {
		t.Fatalf("inserting the company: %v", err)
	}

	var vacancyID string
	if err := st.Pool().QueryRow(ctx, `
		INSERT INTO vacancies (source, source_id, title, company_id, city, remote,
		                       language, raw_text, fingerprint, status)
		VALUES ('test', $1, 'Senior Backend Engineer', $2, 'Freiburg', 'hybrid',
		        'de', $3, $4, 'scored')
		RETURNING id`,
		fmt.Sprintf("test-%d", stamp), companyID,
		"Wir suchen eine erfahrene Person für Go und PostgreSQL.",
		fmt.Sprintf("test-fp-%d", stamp),
	).Scan(&vacancyID); err != nil {
		t.Fatalf("inserting the vacancy: %v", err)
	}

	t.Cleanup(func() {
		ctx := context.Background()
		//nolint:errcheck // best-effort cleanup; the cascade does the rest
		st.Pool().Exec(ctx, `DELETE FROM vacancies WHERE id = $1`, vacancyID)
		//nolint:errcheck // best-effort cleanup
		st.Pool().Exec(ctx, `DELETE FROM companies WHERE id = $1`, companyID)
	})

	if err := st.SaveScore(ctx, store.Scored{
		VacancyID: vacancyID,
		Requirements: []scoring.Requirement{
			{Text: "Mehrjährige Erfahrung mit Go", Kind: scoring.Must, IDs: []string{"go"}, Coverage: 1},
			{Text: "PostgreSQL im Betrieb", Kind: scoring.Must, IDs: []string{"postgres"}, Coverage: 1},
			{Text: "Kubernetes", Kind: scoring.Nice, Coverage: 0},
		},
		Categories:      []string{"language", "db", "cloud"},
		Result:          result,
		Model:           "test",
		InventoryDigest: "test",
	}); err != nil {
		t.Fatalf("SaveScore: %v", err)
	}
	return vacancyID
}

func passing() scoring.Result {
	return scoring.Result{Score: 88, MustCoverage: 1, MustCount: 2, NiceCoverage: 0, NiceCount: 1}
}

type harness struct {
	deps  Deps
	rt    *runtime.Runtime
	model *llm.Fake
	agent runtime.Agent
	dir   string
}

func setup(t *testing.T, st *store.Store) *harness {
	t.Helper()

	inv, err := inventory.Parse([]byte(invYAML))
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	prof, err := profile.Parse([]byte(profYAML))
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	sty, err := style.Parse([]byte(styleYAML))
	if err != nil {
		t.Fatalf("style: %v", err)
	}

	model := llm.NewFake()
	model.Reply("only", json.RawMessage(draftJSON))

	root := t.TempDir()
	dir := filepath.Join(root, "letters")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "build_letter.py"))
	if err != nil {
		t.Fatal(err)
	}

	deps := Deps{
		Store: st, Model: model, Inventory: inv, Profile: prof, Style: sty,
		RepoRoot: root, LettersDir: dir, Script: script,
	}

	rt := runtime.New(registry.New(st.Pool()), st.Calls())
	if _, err := Install(context.Background(), registry.New(st.Pool()), rt, deps,
		[]string{DraftLetter}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	agentID, err := st.EnsureAgent(context.Background(), "test-writer",
		[]string{"vacancies:read", "scores:read", "letters:write", "tools:call"})
	if err != nil {
		t.Fatalf("EnsureAgent: %v", err)
	}

	return &harness{
		deps: deps, rt: rt, model: model, dir: dir,
		agent: runtime.Agent{ID: agentID, Name: "test-writer",
			Scopes: []string{"vacancies:read", "scores:read", "letters:write", "tools:call"}},
	}
}

func input(t *testing.T, vacancyID string, contact ...string) json.RawMessage {
	t.Helper()
	in := map[string]string{"vacancy_id": vacancyID}
	if len(contact) == 3 {
		in["contact_first_name"], in["contact_last_name"], in["contact_gender"] =
			contact[0], contact[1], contact[2]
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type output struct {
	LetterID  string   `json:"letter_id"`
	Status    string   `json:"status"`
	TxtPath   string   `json:"txt_path"`
	PDFPath   string   `json:"pdf_path"`
	FactsUsed []string `json:"facts_used"`
	Words     int      `json:"words"`
	Reason    string   `json:"reason"`
}

// The whole of SPEC.md §13 in one call: eligible vacancy in, a fact-locked
// letter on disk and a row pointing at it out.
func TestDraftLetterEndToEnd(t *testing.T) {
	st := openTest(t)
	h := setup(t, st)
	vacancyID := scoredVacancy(t, st, passing())

	raw, err := h.rt.Execute(context.Background(), runtime.Request{
		Agent: h.agent, Tool: DraftLetter, Input: input(t, vacancyID, "Kim", "Weber", ""),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var out output
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding the result: %v", err)
	}
	if out.Status != letters.StatusDraft {
		t.Errorf("status %q with reason %q, want draft", out.Status, out.Reason)
	}
	if strings.Join(out.FactsUsed, ",") != "go,postgres" {
		t.Errorf("facts_used = %v", out.FactsUsed)
	}

	// SPEC.md §13.5's filename, and a path relative to the working tree:
	// an absolute one would record a home directory in the table.
	if want := "letters/cover-letter_NordwindLogistikGmbH_Beispiel_Alex.txt"; out.TxtPath != want {
		t.Errorf("txt_path = %q, want %q", out.TxtPath, want)
	}
	if filepath.IsAbs(out.TxtPath) || filepath.IsAbs(out.PDFPath) {
		t.Error("an absolute path was stored")
	}

	body, err := os.ReadFile(filepath.Join(h.deps.RepoRoot, out.TxtPath))
	if err != nil {
		t.Fatalf("reading the draft: %v", err)
	}
	// The salutation is the style file's, not the model's, and the
	// advertisement gives no gender for Kim Weber.
	if !strings.HasPrefix(string(body), "Guten Tag Kim Weber,") {
		t.Errorf("the letter opens with %q", strings.SplitN(string(body), "\n", 2)[0])
	}
	if strings.Contains(string(body), "wird ersetzt") {
		t.Error("the model's placeholder salutation survived")
	}
	if !strings.HasSuffix(strings.TrimSpace(string(body)), "Mit freundlichen Grüßen") {
		t.Error("the letter does not close with the style file's line")
	}

	// The row, and the vacancy it moved on.
	var (
		status, txtPath string
		pdfPath         *string
		facts           []string
	)
	if err := st.Pool().QueryRow(context.Background(), `
		SELECT status, txt_path, pdf_path, facts_used FROM letters WHERE vacancy_id = $1`,
		vacancyID).Scan(&status, &txtPath, &pdfPath, &facts); err != nil {
		t.Fatalf("reading the letters row: %v", err)
	}
	if status != "draft" || txtPath != out.TxtPath {
		t.Errorf("row has status %q and path %q", status, txtPath)
	}
	if strings.Join(facts, ",") != "go,postgres" {
		t.Errorf("facts_used in the row = %v", facts)
	}

	var vacancyStatus string
	if err := st.Pool().QueryRow(context.Background(),
		`SELECT status FROM vacancies WHERE id = $1`, vacancyID).Scan(&vacancyStatus); err != nil {
		t.Fatal(err)
	}
	if vacancyStatus != "letter_drafted" {
		t.Errorf("the vacancy is still %q", vacancyStatus)
	}
}

// Redrafting replaces the row and the file. Two drafts for one vacancy is
// two letters that could be sent, and only one of them is current.
func TestRedraftingReplaces(t *testing.T) {
	st := openTest(t)
	h := setup(t, st)
	vacancyID := scoredVacancy(t, st, passing())

	var first output
	for i := 0; i < 2; i++ {
		raw, err := h.rt.Execute(context.Background(), runtime.Request{
			Agent: h.agent, Tool: DraftLetter, Input: input(t, vacancyID),
		})
		if err != nil {
			t.Fatalf("Execute %d: %v", i+1, err)
		}
		var out output
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = out
		} else if out.LetterID != first.LetterID {
			t.Errorf("a second draft produced a second row: %s then %s", first.LetterID, out.LetterID)
		}
	}

	var rows int
	if err := st.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM letters WHERE vacancy_id = $1`, vacancyID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("%d rows for one vacancy", rows)
	}
}

// A vacancy that does not warrant a letter is refused with a code the
// queue will not retry, and the model is never reached.
func TestIneligibleVacancyIsRefusedPermanently(t *testing.T) {
	st := openTest(t)

	cases := map[string]scoring.Result{
		"below the threshold": {Score: 61, MustCoverage: 0.5, MustCount: 2},
		"an uncovered must":   {Score: 95, MustCount: 2, MustGaps: []string{"Kubernetes"}},
	}

	for name, result := range cases {
		t.Run(name, func(t *testing.T) {
			h := setup(t, st)
			vacancyID := scoredVacancy(t, st, result)

			_, err := h.rt.Execute(context.Background(), runtime.Request{
				Agent: h.agent, Tool: DraftLetter, Input: input(t, vacancyID),
			})
			if err == nil {
				t.Fatal("a letter was drafted")
			}
			if code := runtime.CodeOf(err); code != runtime.CodeInvalidInput {
				t.Errorf("error code %q, want invalid_input so the queue stops retrying", code)
			}
			if len(h.model.Calls) != 0 {
				t.Errorf("the model was called %d times", len(h.model.Calls))
			}

			var rows int
			if err := st.Pool().QueryRow(context.Background(),
				`SELECT count(*) FROM letters WHERE vacancy_id = $1`, vacancyID).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if rows != 0 {
				t.Error("a row was written for a letter that was refused")
			}
		})
	}
}

// A vacancy that does not exist is the same answer: stop, do not retry.
func TestUnknownVacancyIsRefusedPermanently(t *testing.T) {
	st := openTest(t)
	h := setup(t, st)

	_, err := h.rt.Execute(context.Background(), runtime.Request{
		Agent: h.agent, Tool: DraftLetter,
		Input: input(t, "00000000-0000-0000-0000-000000000000"),
	})
	if runtime.CodeOf(err) != runtime.CodeInvalidInput {
		t.Errorf("got %v, want invalid_input", err)
	}
	if !errors.Is(err, store.ErrNoScore) {
		t.Errorf("the error does not say the vacancy has no score: %v", err)
	}
}

// The scope check is the point of registering the tool rather than
// calling the handler: an analyst holds no letters:write and cannot draft
// even by naming the tool directly.
func TestARoleWithoutLettersWriteIsRefused(t *testing.T) {
	st := openTest(t)
	h := setup(t, st)
	vacancyID := scoredVacancy(t, st, passing())

	analyst := runtime.Agent{
		ID: h.agent.ID, Name: "analyst",
		Scopes: []string{"vacancies:read", "inventory:read", "scores:write", "tools:call"},
	}
	_, err := h.rt.Execute(context.Background(), runtime.Request{
		Agent: analyst, Tool: DraftLetter, Input: input(t, vacancyID),
	})
	if code := runtime.CodeOf(err); code != runtime.CodeForbidden {
		t.Errorf("error code %q, want forbidden", code)
	}
	if len(h.model.Calls) != 0 {
		t.Error("the model was reached by an agent without the scope")
	}
}

// An argument the schema does not allow never reaches the handler.
func TestInventedArgumentsAreRefusedBySchema(t *testing.T) {
	st := openTest(t)
	h := setup(t, st)
	vacancyID := scoredVacancy(t, st, passing())

	_, err := h.rt.Execute(context.Background(), runtime.Request{
		Agent: h.agent, Tool: DraftLetter,
		Input: json.RawMessage(fmt.Sprintf(
			`{"vacancy_id":%q,"tone":"enthusiastic"}`, vacancyID)),
	})
	if code := runtime.CodeOf(err); code != runtime.CodeInvalidInput {
		t.Errorf("error code %q, want invalid_input", code)
	}
}

// The step id is the idempotency key, so a step retried after its handler
// already finished replays rather than drafting a second letter and
// spending a second model call on it.
func TestARetriedStepReplaysInsteadOfRedrafting(t *testing.T) {
	st := openTest(t)
	h := setup(t, st)
	vacancyID := scoredVacancy(t, st, passing())

	key := fmt.Sprintf("%d", time.Now().UnixNano())
	var first json.RawMessage
	for i := 0; i < 2; i++ {
		out, err := h.rt.Execute(context.Background(), runtime.Request{
			Agent: h.agent, Tool: DraftLetter, Input: input(t, vacancyID),
			IdempotencyKey: key,
		})
		if err != nil {
			t.Fatalf("Execute %d: %v", i+1, err)
		}
		if i == 0 {
			first = out
		} else if string(out) != string(first) {
			t.Errorf("the replay returned something else:\n%s\n%s", first, out)
		}
	}
	if len(h.model.Calls) != 1 {
		t.Errorf("the model was called %d times for one step", len(h.model.Calls))
	}
}
