// Package registry is the single source of truth for what tools exist,
// what shape their input and output take, and who may call them.
//
// Validation happens here rather than in a prompt, because a prompt can be
// ignored. A model that invents an argument gets a validation error back as
// a tool result it can react to; it never reaches a handler and it never
// panics the process.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Errors a caller is expected to distinguish.
var (
	ErrNotFound      = errors.New("tool not found")
	ErrSchemaPolicy  = errors.New("schema does not meet the registry's policy")
	ErrInvalidInput  = errors.New("input does not match the tool's schema")
	ErrInvalidOutput = errors.New("output does not match the tool's schema")
)

// Definition is a tool as submitted for registration.
type Definition struct {
	Name           string
	Description    string
	InputSchema    json.RawMessage
	OutputSchema   json.RawMessage
	RequiredScopes []string
	Timeout        time.Duration
}

// Tool is a registered version of a tool.
type Tool struct {
	Name           string
	Version        int
	Description    string
	InputSchema    json.RawMessage
	OutputSchema   json.RawMessage
	RequiredScopes []string
	Timeout        time.Duration

	in, out *jsonschema.Schema
}

// NewTool compiles a definition into a usable tool without storing it.
//
// Built-in tools that exist in code rather than in the table - call_tool
// among them - need a compiled Tool to be validated against, and so do
// tests that have no database. The policy and the metaschema are applied
// exactly as Register applies them, so a tool built this way cannot be
// looser than one that was stored.
func NewTool(def Definition, version int) (*Tool, error) {
	if strings.TrimSpace(def.Name) == "" {
		return nil, fmt.Errorf("registry: empty tool name")
	}
	if err := CheckSchemaPolicy(def.InputSchema); err != nil {
		return nil, fmt.Errorf("registry: %s input: %w", def.Name, err)
	}
	if err := CheckSchemaPolicy(def.OutputSchema); err != nil {
		return nil, fmt.Errorf("registry: %s output: %w", def.Name, err)
	}
	if version <= 0 {
		version = 1
	}
	if def.Timeout <= 0 {
		def.Timeout = defaultTimeout
	}
	if def.RequiredScopes == nil {
		def.RequiredScopes = []string{}
	}

	t := &Tool{
		Name: def.Name, Version: version, Description: def.Description,
		InputSchema: def.InputSchema, OutputSchema: def.OutputSchema,
		RequiredScopes: def.RequiredScopes, Timeout: def.Timeout,
	}

	var err error
	if t.in, err = compile(def.Name+"-input", def.InputSchema); err != nil {
		return nil, err
	}
	if t.out, err = compile(def.Name+"-output", def.OutputSchema); err != nil {
		return nil, err
	}
	return t, nil
}

// Registry stores and resolves tools.
type Registry struct {
	pool *pgxpool.Pool

	mu       sync.RWMutex
	compiled map[string]*Tool // name@version
}

// New returns a registry backed by the given pool.
func New(pool *pgxpool.Pool) *Registry {
	return &Registry{pool: pool, compiled: map[string]*Tool{}}
}

// defaultTimeout matches SPEC.md §6.
const defaultTimeout = 5 * time.Second

// Register stores a tool, bumping its version when one already exists.
//
// The previous version stays callable: a task in flight pinned it, and
// changing a tool under a running task is how a replay stops matching its
// log.
func (r *Registry) Register(ctx context.Context, def Definition) (*Tool, error) {
	if strings.TrimSpace(def.Name) == "" {
		return nil, fmt.Errorf("registry: empty tool name")
	}
	if err := CheckSchemaPolicy(def.InputSchema); err != nil {
		return nil, fmt.Errorf("registry: %s input: %w", def.Name, err)
	}
	if err := CheckSchemaPolicy(def.OutputSchema); err != nil {
		return nil, fmt.Errorf("registry: %s output: %w", def.Name, err)
	}
	if def.Timeout <= 0 {
		def.Timeout = defaultTimeout
	}
	if def.RequiredScopes == nil {
		def.RequiredScopes = []string{}
	}

	// Compiled before the insert, not after. The policy check is a coarse
	// filter; only the compiler knows whether a schema is valid against the
	// metaschema, and a row that cannot be compiled is a row no caller can
	// ever load - including the listing, which would then fail for every
	// tool because of one.
	if _, err := compile(def.Name+"-input", def.InputSchema); err != nil {
		return nil, err
	}
	if _, err := compile(def.Name+"-output", def.OutputSchema); err != nil {
		return nil, err
	}

	var version int
	err := r.pool.QueryRow(ctx, `
		INSERT INTO tools (name, version, description, input_schema,
		                   output_schema, required_scopes, timeout_ms)
		VALUES ($1,
		        COALESCE((SELECT max(version) FROM tools WHERE name = $1), 0) + 1,
		        NULLIF($2, ''), $3, $4, $5, $6)
		RETURNING version`,
		def.Name, def.Description, def.InputSchema, def.OutputSchema,
		def.RequiredScopes, def.Timeout.Milliseconds(),
	).Scan(&version)
	if err != nil {
		return nil, fmt.Errorf("registry: registering %s: %w", def.Name, err)
	}

	return r.Resolve(ctx, def.Name, version)
}

// Ensure registers a tool only when it is not already stored as it stands.
//
// Register bumps the version every time it is called, which is right for a
// tool that changed and wrong for a process that registers its tools at
// startup: three roles restarting twice a day would walk the version
// number up for tools nobody touched, and every one of those versions is
// a row a trace might pin. Ensure compares the definition with the latest
// stored one and registers only on a difference.
func (r *Registry) Ensure(ctx context.Context, def Definition) (*Tool, error) {
	current, err := r.Resolve(ctx, def.Name, 0)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if current != nil && current.matches(def) {
		return current, nil
	}
	return r.Register(ctx, def)
}

// matches reports whether a stored tool is the definition unchanged.
//
// The schemas are compared as values rather than as bytes: they went into
// jsonb, which does not promise to give back the key order or the
// whitespace they went in with, so a byte comparison would report a
// difference on every startup.
func (t *Tool) matches(def Definition) bool {
	timeout := def.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	scopes := def.RequiredScopes
	if scopes == nil {
		scopes = []string{}
	}

	return t.Description == def.Description &&
		t.Timeout == timeout &&
		slices.Equal(t.RequiredScopes, scopes) &&
		sameJSON(t.InputSchema, def.InputSchema) &&
		sameJSON(t.OutputSchema, def.OutputSchema)
}

func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &y); err != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// Resolve returns a tool. Version 0 means the latest.
func (r *Registry) Resolve(ctx context.Context, name string, version int) (*Tool, error) {
	if version > 0 {
		if t := r.cached(key(name, version)); t != nil {
			return t, nil
		}
	}

	row := r.pool.QueryRow(ctx, `
		SELECT name, version, coalesce(description, ''), input_schema,
		       output_schema, required_scopes, timeout_ms
		FROM tools
		WHERE name = $1 AND ($2 = 0 OR version = $2)
		ORDER BY version DESC
		LIMIT 1`, name, version)

	t, err := scanTool(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("registry: %q: %w", name, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	r.compiled[key(t.Name, t.Version)] = t
	r.mu.Unlock()
	return t, nil
}

// ListFor returns the tools callable with the given scopes, sorted by name.
//
// The sort is not cosmetic. The list goes into a model request, and a
// stable order keeps the prompt-cache prefix stable; a map's iteration
// order would silently cost a cache hit on every call.
func (r *Registry) ListFor(ctx context.Context, scopes []string) ([]*Tool, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT DISTINCT ON (name)
		       name, version, coalesce(description, ''), input_schema,
		       output_schema, required_scopes, timeout_ms
		FROM tools
		ORDER BY name, version DESC`)
	if err != nil {
		return nil, fmt.Errorf("registry: listing tools: %w", err)
	}
	defer rows.Close()

	held := make(map[string]bool, len(scopes))
	for _, s := range scopes {
		held[s] = true
	}

	var out []*Tool
	for rows.Next() {
		t, err := scanTool(rows)
		if err != nil {
			// A row stored before the compiler ran at registration time
			// cannot be loaded. Skipping it keeps the rest of the catalogue
			// usable; failing here would hide every working tool behind one
			// broken record.
			slog.Warn("registry: skipping an unloadable tool", "error", err)
			continue
		}
		if covers(held, t.RequiredScopes) {
			out = append(out, t)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Permits reports whether the scopes held cover what the tool requires.
func (t *Tool) Permits(scopes []string) bool {
	held := make(map[string]bool, len(scopes))
	for _, s := range scopes {
		held[s] = true
	}
	return covers(held, t.RequiredScopes)
}

func covers(held map[string]bool, required []string) bool {
	for _, need := range required {
		if !held[need] {
			return false
		}
	}
	return true
}

// ValidateInput checks an argument object before the handler runs.
func (t *Tool) ValidateInput(raw json.RawMessage) error {
	return validate(t.in, raw, ErrInvalidInput)
}

// ValidateOutput checks a result after the handler returns, so a handler
// that drifts from its contract is caught here and not by its caller.
func (t *Tool) ValidateOutput(raw json.RawMessage) error {
	return validate(t.out, raw, ErrInvalidOutput)
}

func validate(schema *jsonschema.Schema, raw json.RawMessage, kind error) error {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("%w: not valid JSON: %v", kind, err)
	}
	if err := schema.Validate(value); err != nil {
		return fmt.Errorf("%w: %v", kind, err)
	}
	return nil
}

// CheckSchemaPolicy enforces what SPEC.md §6 requires of every schema: an
// object that rejects unknown properties and states which are required.
//
// Without additionalProperties:false a model can add an argument nobody
// validates and nobody reads, and the call looks successful.
func CheckSchemaPolicy(raw json.RawMessage) error {
	if len(raw) == 0 {
		return fmt.Errorf("%w: schema is empty", ErrSchemaPolicy)
	}

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("%w: not a JSON object: %v", ErrSchemaPolicy, err)
	}
	if doc["type"] != "object" {
		return fmt.Errorf(`%w: type must be "object", got %v`, ErrSchemaPolicy, doc["type"])
	}
	if extra, ok := doc["additionalProperties"]; !ok || extra != false {
		return fmt.Errorf(`%w: additionalProperties must be false`, ErrSchemaPolicy)
	}
	if _, ok := doc["required"]; !ok {
		return fmt.Errorf(`%w: "required" must be listed, even if empty`, ErrSchemaPolicy)
	}
	required, ok := doc["required"].([]any)
	if !ok {
		return fmt.Errorf(`%w: "required" must be an array`, ErrSchemaPolicy)
	}
	// Caught here rather than by the metaschema at compile time, which
	// reports only that validation failed and not which key is at fault.
	for i, name := range required {
		if _, ok := name.(string); !ok {
			return fmt.Errorf(`%w: required[%d] must be a property name, got %T`,
				ErrSchemaPolicy, i, name)
		}
	}
	return nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanTool(row scanner) (*Tool, error) {
	var (
		t       Tool
		timeout int
	)
	if err := row.Scan(&t.Name, &t.Version, &t.Description, &t.InputSchema,
		&t.OutputSchema, &t.RequiredScopes, &timeout); err != nil {
		return nil, err
	}
	t.Timeout = time.Duration(timeout) * time.Millisecond

	var err error
	if t.in, err = compile(t.Name+"-input", t.InputSchema); err != nil {
		return nil, err
	}
	if t.out, err = compile(t.Name+"-output", t.OutputSchema); err != nil {
		return nil, err
	}
	return &t, nil
}

func compile(id string, raw json.RawMessage) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("registry: %s: %w", id, err)
	}
	c := jsonschema.NewCompiler()
	url := "mem://" + id + ".json"
	if err := c.AddResource(url, doc); err != nil {
		return nil, fmt.Errorf("registry: %s: %w", id, err)
	}
	s, err := c.Compile(url)
	if err != nil {
		return nil, fmt.Errorf("registry: %s: %w", id, err)
	}
	return s, nil
}

func key(name string, version int) string { return fmt.Sprintf("%s@%d", name, version) }

func (r *Registry) cached(k string) *Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.compiled[k]
}
