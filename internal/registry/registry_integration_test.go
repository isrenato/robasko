package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func schema(required, properties string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"type":"object","additionalProperties":false,"required":[%s],"properties":{%s}}`,
		required, properties))
}

func openTest(t *testing.T) *Registry {
	t.Helper()
	dsn := os.Getenv("ROBASKO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ROBASKO_TEST_DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return New(pool)
}

// Each test registers under its own name, so runs do not collide.
func uniqueName(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("test.%s.%d", t.Name(), time.Now().UnixNano())
}

func TestRegisterThenResolve(t *testing.T) {
	r := openTest(t)
	ctx := context.Background()
	name := uniqueName(t)

	registered, err := r.Register(ctx, Definition{
		Name:           name,
		Description:    "extracts requirements",
		InputSchema:    schema(`"vacancy_id"`, `"vacancy_id":{"type":"string"}`),
		OutputSchema:   schema(`"requirements"`, `"requirements":{"type":"array"}`),
		RequiredScopes: []string{"vacancies:read"},
		Timeout:        2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if registered.Version != 1 {
		t.Errorf("first registration got version %d, want 1", registered.Version)
	}
	if registered.Timeout != 2*time.Second {
		t.Errorf("timeout %v, want 2s", registered.Timeout)
	}

	resolved, err := r.Resolve(ctx, name, 0)
	if err != nil {
		t.Fatalf("Resolve latest: %v", err)
	}
	if resolved.Version != 1 || resolved.Description != "extracts requirements" {
		t.Errorf("unexpected tool: %+v", resolved)
	}
}

// Registering a changed tool bumps the version, and the old one stays
// callable: a task in flight pinned it, and changing a tool underneath a
// running task is how a replay stops matching its own log.
func TestVersionBumpKeepsTheOldVersionCallable(t *testing.T) {
	r := openTest(t)
	ctx := context.Background()
	name := uniqueName(t)

	first := Definition{
		Name:         name,
		InputSchema:  schema(`"a"`, `"a":{"type":"string"}`),
		OutputSchema: schema(``, ``),
	}
	if _, err := r.Register(ctx, first); err != nil {
		t.Fatalf("first Register: %v", err)
	}

	second := first
	second.InputSchema = schema(`"a","b"`, `"a":{"type":"string"},"b":{"type":"integer"}`)
	bumped, err := r.Register(ctx, second)
	if err != nil {
		t.Fatalf("second Register: %v", err)
	}
	if bumped.Version != 2 {
		t.Fatalf("got version %d, want 2", bumped.Version)
	}

	old, err := r.Resolve(ctx, name, 1)
	if err != nil {
		t.Fatalf("version 1 is no longer resolvable: %v", err)
	}
	// Version 1 did not know about "b", and must still reject it.
	if err := old.ValidateInput(json.RawMessage(`{"a":"x","b":1}`)); err == nil {
		t.Error("version 1 accepted an argument added in version 2")
	}
	if err := bumped.ValidateInput(json.RawMessage(`{"a":"x","b":1}`)); err != nil {
		t.Errorf("version 2 rejected its own argument: %v", err)
	}
}

func TestResolveReportsAnUnknownTool(t *testing.T) {
	r := openTest(t)
	if _, err := r.Resolve(context.Background(), "test.nonexistent.tool", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestValidationRejectsUnknownAndMissingProperties(t *testing.T) {
	r := openTest(t)
	tool, err := r.Register(context.Background(), Definition{
		Name:         uniqueName(t),
		InputSchema:  schema(`"vacancy_id"`, `"vacancy_id":{"type":"string"}`),
		OutputSchema: schema(`"score"`, `"score":{"type":"number"}`),
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	cases := map[string]json.RawMessage{
		"unknown property": json.RawMessage(`{"vacancy_id":"v1","extra":true}`),
		"missing required": json.RawMessage(`{}`),
		"wrong type":       json.RawMessage(`{"vacancy_id":42}`),
		"not an object":    json.RawMessage(`"v1"`),
		"not json":         json.RawMessage(`{`),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			err := tool.ValidateInput(input)
			if err == nil {
				t.Fatal("accepted")
			}
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("got %v, want ErrInvalidInput", err)
			}
		})
	}

	if err := tool.ValidateInput(json.RawMessage(`{"vacancy_id":"v1"}`)); err != nil {
		t.Errorf("rejected valid input: %v", err)
	}
}

// A handler that drifts from its contract is caught here, not by whoever
// consumes its result.
func TestOutputIsValidatedToo(t *testing.T) {
	r := openTest(t)
	tool, err := r.Register(context.Background(), Definition{
		Name:         uniqueName(t),
		InputSchema:  schema(``, ``),
		OutputSchema: schema(`"score"`, `"score":{"type":"number"}`),
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := tool.ValidateOutput(json.RawMessage(`{"score":"high"}`)); !errors.Is(err, ErrInvalidOutput) {
		t.Errorf("got %v, want ErrInvalidOutput", err)
	}
	if err := tool.ValidateOutput(json.RawMessage(`{"score":83.3}`)); err != nil {
		t.Errorf("rejected valid output: %v", err)
	}
}

func TestRegisterRefusesASchemaOutsidePolicy(t *testing.T) {
	r := openTest(t)
	_, err := r.Register(context.Background(), Definition{
		Name:         uniqueName(t),
		InputSchema:  json.RawMessage(`{"type":"object","required":[]}`), // no additionalProperties
		OutputSchema: schema(``, ``),
	})
	if !errors.Is(err, ErrSchemaPolicy) {
		t.Errorf("got %v, want ErrSchemaPolicy", err)
	}
}

// The list goes into a model request, so its order has to be stable: a
// map's iteration order would cost a prompt-cache hit on every call.
func TestListForIsScopedAndSorted(t *testing.T) {
	r := openTest(t)
	ctx := context.Background()
	stamp := time.Now().UnixNano()

	for _, spec := range []struct {
		suffix string
		scopes []string
	}{
		{"zebra", []string{"vacancies:read"}},
		{"alpha", []string{"vacancies:read"}},
		{"secret", []string{"letters:write"}},
	} {
		if _, err := r.Register(ctx, Definition{
			Name:           fmt.Sprintf("test.list.%d.%s", stamp, spec.suffix),
			InputSchema:    schema(``, ``),
			OutputSchema:   schema(``, ``),
			RequiredScopes: spec.scopes,
		}); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}

	t.Cleanup(func() {
		r.pool.Exec(context.Background(),
			`DELETE FROM tools WHERE name LIKE $1`, fmt.Sprintf("test.list.%d.%%", stamp))
	})

	tools, err := r.ListFor(ctx, []string{"vacancies:read"})
	if err != nil {
		t.Fatalf("ListFor: %v", err)
	}

	// Scoped to this run's stamp: the prefix alone also matches the rows
	// every previous run left behind, which turns a passing test into one
	// that only works on an empty database.
	prefix := fmt.Sprintf("test.list.%d.", stamp)
	var mine []string
	for _, tool := range tools {
		if strings.HasPrefix(tool.Name, prefix) {
			mine = append(mine, tool.Name)
		}
	}
	if len(mine) != 2 {
		t.Fatalf("got %v, want the two readable tools", mine)
	}
	if mine[0] >= mine[1] {
		t.Errorf("not sorted by name: %v", mine)
	}
	for _, name := range mine {
		if name[len(name)-6:] == "secret" {
			t.Error("a tool outside the agent's scopes was listed")
		}
	}
}

// Ensure exists so a process can register its tools at startup without
// walking the version number up every time it restarts. Every one of
// those versions would be a row a trace might pin.
func TestEnsureDoesNotBumpAnUnchangedTool(t *testing.T) {
	r := openTest(t)
	ctx := context.Background()

	def := Definition{
		Name:           uniqueName(t),
		Description:    "drafts a letter",
		InputSchema:    schema(`"vacancy_id"`, `"vacancy_id":{"type":"string"}`),
		OutputSchema:   schema(`"letter_id"`, `"letter_id":{"type":"string"}`),
		RequiredScopes: []string{"letters:write"},
		Timeout:        4 * time.Minute,
	}

	first, err := r.Ensure(ctx, def)
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	if first.Version != 1 {
		t.Fatalf("first registration got version %d, want 1", first.Version)
	}

	// A second registry over the same pool, because the first has the
	// tool compiled in memory and would answer from there.
	for i := 0; i < 3; i++ {
		again, err := New(r.pool).Ensure(ctx, def)
		if err != nil {
			t.Fatalf("Ensure %d: %v", i+2, err)
		}
		if again.Version != 1 {
			t.Fatalf("re-registering an unchanged tool produced version %d", again.Version)
		}
	}
}

// A tool that really did change still gets a new version, and the old one
// stays callable because a task in flight pinned it.
func TestEnsureBumpsAChangedTool(t *testing.T) {
	r := openTest(t)
	ctx := context.Background()
	name := uniqueName(t)

	base := Definition{
		Name:           name,
		Description:    "drafts a letter",
		InputSchema:    schema(`"vacancy_id"`, `"vacancy_id":{"type":"string"}`),
		OutputSchema:   schema(`"letter_id"`, `"letter_id":{"type":"string"}`),
		RequiredScopes: []string{"letters:write"},
		Timeout:        time.Minute,
	}
	if _, err := r.Ensure(ctx, base); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	changes := map[string]func(*Definition){
		"the description": func(d *Definition) { d.Description = "drafts a cover letter" },
		"a scope":         func(d *Definition) { d.RequiredScopes = []string{"letters:write", "scores:read"} },
		"the timeout":     func(d *Definition) { d.Timeout = 2 * time.Minute },
		"the input schema": func(d *Definition) {
			d.InputSchema = schema(`"vacancy_id"`, `"vacancy_id":{"type":"string"},"contact":{"type":"string"}`)
		},
	}

	version := 1
	for what, change := range changes {
		def := base
		change(&def)
		got, err := New(r.pool).Ensure(ctx, def)
		if err != nil {
			t.Fatalf("Ensure after changing %s: %v", what, err)
		}
		version++
		if got.Version != version {
			t.Errorf("changing %s produced version %d, want %d", what, got.Version, version)
		}
		base = def
	}

	// Version 1 is still there to be resolved.
	old, err := New(r.pool).Resolve(ctx, name, 1)
	if err != nil {
		t.Fatalf("resolving the first version: %v", err)
	}
	if old.Description != "drafts a letter" {
		t.Errorf("version 1 changed under a task that pinned it: %q", old.Description)
	}
}
