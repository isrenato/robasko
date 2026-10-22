package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"

	"github.com/isrenato/robasko/internal/inventory"
	"github.com/isrenato/robasko/internal/llm"
	"github.com/isrenato/robasko/internal/orchestrator"
	"github.com/isrenato/robasko/internal/paths"
	"github.com/isrenato/robasko/internal/profile"
	"github.com/isrenato/robasko/internal/registry"
	"github.com/isrenato/robasko/internal/runtime"
	"github.com/isrenato/robasko/internal/store"
	"github.com/isrenato/robasko/internal/style"
	"github.com/isrenato/robasko/internal/tools"
)

// domainTools registers the tools this role claims and attaches their
// handlers, returning the names that are now callable.
//
// Only what the role handles is loaded. A scout that read the profile and
// the inventory at startup would hold a copy of the candidate's biography
// in a process with no use for one, and the point of giving each role its
// own process is that each holds only what its job needs.
func domainTools(ctx context.Context, st *store.Store, rt *runtime.Runtime,
	role orchestrator.Role, dataRoot string, log *slog.Logger) ([]string, error) {

	deps := tools.Deps{Store: st, RepoRoot: repoRoot(), Log: log}

	if slices.Contains(role.Handles, tools.DraftLetter) {
		if err := loadForLetters(&deps, dataRoot, log); err != nil {
			return nil, err
		}
	}

	return tools.Install(ctx, registry.New(st.Pool()), rt, deps, role.Handles)
}

// loadForLetters fills in what drafting needs, and fails loudly when any
// of it is missing. A writer that starts without a profile is a writer
// that accepts work it will refuse one step later, after the queue has
// already counted an attempt against it.
func loadForLetters(d *tools.Deps, dataRoot string, log *slog.Logger) error {
	var err error

	if d.Inventory, err = inventory.Load(privateFile(dataRoot, "inventory.yaml")); err != nil {
		return err
	}
	if d.Profile, err = profile.Load(privateFile(dataRoot, "profile.yaml")); err != nil {
		return err
	}
	if d.Style, err = style.Load(privateFile(dataRoot, "style.yaml")); err != nil {
		return err
	}
	for _, warning := range d.Style.Warnings() {
		log.Warn("style file", "warning", warning)
	}

	if d.Model, err = llm.New(llm.Options{
		APIKey:      os.Getenv("ANTHROPIC_API_KEY"),
		BaseURL:     os.Getenv("ANTHROPIC_BASE_URL"),
		Model:       os.Getenv("ANTHROPIC_MODEL"),
		WorkspaceID: os.Getenv("ANTHROPIC_WORKSPACE_ID"),
	}); err != nil {
		return err
	}

	// paths.Letters refuses to return a directory git would track, and it
	// is called here rather than at the first draft: a writer that cannot
	// write safely should not claim work.
	if d.LettersDir, err = paths.Letters(repoRoot()); err != nil {
		return err
	}

	// The renderer is optional. The runtime image is distroless and has no
	// python, so this path often does not exist, and a letter without a
	// PDF beside it is still a letter.
	script := filepath.Join(repoRoot(), "scripts", "build_letter.py")
	if _, err := os.Stat(script); err == nil {
		d.Script = script
	} else {
		log.Info("no PDF renderer; drafts will be written as .txt only", "looked_for", script)
	}
	return nil
}

// privateFile locates one of the hand-maintained YAML files. The data root
// holds the real ones; the working tree holds symlinks to them for the
// commands run from a checkout, and in a container only the first exists.
func privateFile(dataRoot, name string) string {
	if dataRoot != "" {
		candidate := filepath.Join(dataRoot, name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return filepath.Join(repoRoot(), name)
}
