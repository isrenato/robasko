// Package tools is where the domain work is attached to the runtime.
//
// Everything below this package refuses rather than acts: the registry
// says what a tool's arguments must look like, the runtime says who may
// call it and how deep, the queue says who claims it. None of them knows
// what a vacancy is. This package is the other half - the handlers that
// do the work - and it is deliberately the only place that depends on
// both the runtime and the domain.
//
// A tool is registered and handled together, by Install. Registering a
// schema nothing handles produces a tool that resolves and then fails with
// not_found at the last moment; attaching a handler to a name nothing
// registered produces a handler no caller can reach. Both are easy to do
// accidentally if the two live apart.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/isrenato/robasko/internal/inventory"
	"github.com/isrenato/robasko/internal/letters"
	"github.com/isrenato/robasko/internal/llm"
	"github.com/isrenato/robasko/internal/profile"
	"github.com/isrenato/robasko/internal/registry"
	"github.com/isrenato/robasko/internal/runtime"
	"github.com/isrenato/robasko/internal/store"
	"github.com/isrenato/robasko/internal/style"
)

// Deps is everything the handlers need. A handler takes no globals, so a
// test can give it a fake model and a scratch directory.
type Deps struct {
	Store *store.Store
	Model llm.Client

	Inventory *inventory.Inventory
	Profile   *profile.Profile
	Style     *style.Style

	// RepoRoot is what the stored paths are relative to.
	RepoRoot string
	// LettersDir is where drafts are written. paths.Letters produces it
	// and refuses to produce one git would track.
	LettersDir string
	// Script is scripts/build_letter.py, or empty where there is no
	// python to run it with - the runtime image, for one.
	Script string

	Log *slog.Logger
}

func (d Deps) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

// Definitions returns every domain tool, keyed by name.
//
// The step tools the queue hands out are in here under the names
// orchestrator uses, so a claimed step resolves to something real.
func Definitions() map[string]registry.Definition {
	return map[string]registry.Definition{
		DraftLetter: draftLetterDefinition(),
	}
}

// DraftLetter is the writer's step tool.
const DraftLetter = "draft_letter"

// Install registers the named tools and attaches their handlers.
//
// Names that are not domain tools are skipped rather than refused: a role
// may claim a step tool this package does not implement yet, and the
// honest result is that the step fails with not_found when it is claimed,
// not that the process will not start.
func Install(ctx context.Context, reg *registry.Registry, rt *runtime.Runtime, d Deps, names []string) ([]string, error) {
	defs := Definitions()
	handlers := map[string]runtime.Handler{
		DraftLetter: d.draftLetter,
	}

	var installed []string
	for _, name := range names {
		def, ok := defs[name]
		if !ok {
			continue
		}
		if _, err := reg.Ensure(ctx, def); err != nil {
			return installed, fmt.Errorf("tools: registering %s: %w", name, err)
		}
		rt.Register(name, handlers[name])
		installed = append(installed, name)
	}
	return installed, nil
}

// draftTimeout is generous because the work behind it is one model call at
// high effort followed by up to three renders. The registry's five-second
// default is sized for a tool that reads a table.
const draftTimeout = 4 * time.Minute

func draftLetterDefinition() registry.Definition {
	return registry.Definition{
		Name: DraftLetter,
		Description: "Draft a cover letter for a scored vacancy, hold every sentence " +
			"to an inventory or profile id, and save it as .txt and .pdf.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"additionalProperties": false,
			"required": ["vacancy_id"],
			"properties": {
				"vacancy_id": {"type": "string", "minLength": 1,
					"description": "the scored vacancy to write about"},
				"contact_first_name": {"type": "string"},
				"contact_last_name": {"type": "string"},
				"contact_gender": {"type": "string", "enum": ["f", "m", ""],
					"description": "left empty when the advertisement does not say, which is addressed without guessing"}
			}
		}`),
		OutputSchema: json.RawMessage(`{
			"type": "object",
			"additionalProperties": false,
			"required": ["letter_id", "status", "txt_path", "facts_used", "words"],
			"properties": {
				"letter_id": {"type": "string"},
				"status": {"type": "string", "enum": ["draft", "needs_human"]},
				"txt_path": {"type": "string"},
				"pdf_path": {"type": "string"},
				"facts_used": {"type": "array", "items": {"type": "string"}},
				"words": {"type": "integer"},
				"cuts": {"type": "array", "items": {"type": "string"}},
				"reason": {"type": "string"}
			}
		}`),
		RequiredScopes: []string{"vacancies:read", "scores:read", "letters:write"},
		Timeout:        draftTimeout,
	}
}

type draftLetterInput struct {
	VacancyID        string `json:"vacancy_id"`
	ContactFirstName string `json:"contact_first_name"`
	ContactLastName  string `json:"contact_last_name"`
	ContactGender    string `json:"contact_gender"`
}

type draftLetterOutput struct {
	LetterID  string   `json:"letter_id"`
	Status    string   `json:"status"`
	TxtPath   string   `json:"txt_path"`
	PDFPath   string   `json:"pdf_path,omitempty"`
	FactsUsed []string `json:"facts_used"`
	Words     int      `json:"words"`
	Cuts      []string `json:"cuts,omitempty"`
	Reason    string   `json:"reason,omitempty"`
}

// draftLetter is SPEC.md §13 end to end: check the vacancy warrants a
// letter, write one, hold it to its sources, render it, store the row.
func (d Deps) draftLetter(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var in draftLetterInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, refuse(runtime.CodeInvalidInput, err)
	}
	if d.Store == nil || d.Model == nil || d.Inventory == nil || d.Profile == nil || d.Style == nil {
		return nil, fmt.Errorf("tools: draft_letter is missing a dependency")
	}

	sub, err := d.Store.LetterSubject(ctx, in.VacancyID)
	if err != nil {
		// A vacancy with no score will have no score on the next attempt
		// either, so this is refused rather than retried.
		return nil, refuse(runtime.CodeInvalidInput, err)
	}

	// Checked here as well as inside letters.Write. The point is the
	// error code: an ineligible vacancy is a permanent refusal and must
	// not spend three attempts and two backoffs discovering that.
	if err := letters.Eligible(sub.Result); err != nil {
		return nil, refuse(runtime.CodeInvalidInput, err)
	}

	lang := sub.Language
	if lang == "" {
		// An advertisement whose language nobody recorded is answered in
		// the language of the market it was posted in, not in English.
		lang = "de"
	}
	contact := style.Contact{
		FirstName: in.ContactFirstName,
		LastName:  in.ContactLastName,
		Gender:    in.ContactGender,
		Company:   sub.Company,
	}

	v := letters.Vacancy{
		Title:            sub.Title,
		Company:          sub.Company,
		City:             sub.City,
		Language:         lang,
		ContactFirstName: in.ContactFirstName,
		ContactLastName:  in.ContactLastName,
		ContactGender:    in.ContactGender,
		AdText:           sub.Text,
		Salutation:       d.Style.Salutation(lang, contact),
		Closing:          d.Style.Closing(lang),
	}

	src := letters.Sources{Inventory: d.Inventory, Profile: d.Profile}
	draft, resp, err := letters.Write(ctx, d.Model, v, sub.Result, sub.Requirements, src, d.Style.Prompt())
	if err != nil {
		return nil, err
	}

	name := letters.FileName(sub.Company, d.Style.Sender.Name)
	rendered, err := letters.Render(ctx, draft, d.LettersDir, name, d.pdfFor(sub, v), d.Style.Weight)
	if err != nil {
		return nil, err
	}

	model := ""
	if resp != nil {
		model = resp.Model
	}
	letterID, err := d.Store.SaveLetter(ctx, store.Letter{
		VacancyID: sub.ID,
		TxtPath:   d.relative(rendered.TxtPath),
		PDFPath:   d.relative(rendered.PDFPath),
		FactsUsed: draft.FactsUsed,
		Status:    rendered.Status,
		Reason:    rendered.Reason,
		Model:     model,
	})
	if err != nil {
		return nil, err
	}

	d.log().Info("letter drafted",
		"vacancy_id", sub.ID, "letter_id", letterID, "status", rendered.Status,
		"words", draft.Words(), "facts", len(draft.FactsUsed),
		"cuts", len(rendered.Cuts), "reason", rendered.Reason)

	return json.Marshal(draftLetterOutput{
		LetterID:  letterID,
		Status:    rendered.Status,
		TxtPath:   d.relative(rendered.TxtPath),
		PDFPath:   d.relative(rendered.PDFPath),
		FactsUsed: draft.FactsUsed,
		Words:     draft.Words(),
		Cuts:      rendered.Cuts,
		Reason:    rendered.Reason,
	})
}

// pdfFor assembles what the renderer needs beyond the body.
func (d Deps) pdfFor(sub *store.LetterSubject, v letters.Vacancy) letters.PDF {
	var recipient []string
	if name := strings.TrimSpace(v.ContactFirstName + " " + v.ContactLastName); name != "" {
		recipient = append(recipient, name)
	}
	if sub.City != "" {
		recipient = append(recipient, sub.City)
	}

	return letters.PDF{
		Script:      d.Script,
		Subject:     d.Style.Subject(v.Language, sub.Title),
		Company:     sub.Company,
		Recipient:   recipient,
		SenderName:  d.Style.Sender.Name,
		SenderLines: d.Style.Sender.Lines,
		Date:        time.Now().Format(d.Style.DateFormat(v.Language)),
	}
}

// relative turns a path under the working tree into the form the letters
// table stores. An absolute path in that column records the home
// directory of whoever ran the draft.
func (d Deps) relative(path string) string {
	if path == "" || d.RepoRoot == "" {
		return path
	}
	rel, err := filepath.Rel(d.RepoRoot, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return rel
}

// refuse wraps an error in the code that stops the queue retrying it.
func refuse(code string, err error) error {
	return &runtime.Error{Code: code, Tool: DraftLetter, Err: err}
}
