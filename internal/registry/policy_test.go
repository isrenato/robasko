package registry

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSchemaPolicyAcceptsAConformingSchema(t *testing.T) {
	ok := json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"required": ["vacancy_id"],
		"properties": {"vacancy_id": {"type": "string"}}
	}`)
	if err := CheckSchemaPolicy(ok); err != nil {
		t.Errorf("rejected a conforming schema: %v", err)
	}
}

// A schema with no required list is accepted as long as the key is present:
// a tool may legitimately take no arguments, but it must say so.
func TestSchemaPolicyAcceptsAnEmptyRequiredList(t *testing.T) {
	ok := json.RawMessage(`{"type":"object","additionalProperties":false,"required":[],"properties":{}}`)
	if err := CheckSchemaPolicy(ok); err != nil {
		t.Errorf("rejected a no-argument schema: %v", err)
	}
}

// Without additionalProperties:false a model can pass an argument nobody
// validates and nobody reads, and the call still looks successful.
func TestSchemaPolicyRejectsWhatItMustReject(t *testing.T) {
	cases := map[string]string{
		"no additionalProperties":   `{"type":"object","required":[]}`,
		"additionalProperties true": `{"type":"object","additionalProperties":true,"required":[]}`,
		"no required":               `{"type":"object","additionalProperties":false}`,
		"required not an array":     `{"type":"object","additionalProperties":false,"required":"id"}`,
		"not an object":             `{"type":"string","additionalProperties":false,"required":[]}`,
		"empty":                     ``,
		"not json":                  `{`,
	}
	for name, schema := range cases {
		t.Run(name, func(t *testing.T) {
			err := CheckSchemaPolicy(json.RawMessage(schema))
			if err == nil {
				t.Fatal("accepted")
			}
			if !errors.Is(err, ErrSchemaPolicy) {
				t.Errorf("got %v, want ErrSchemaPolicy", err)
			}
		})
	}
}

func TestPermitsRequiresEveryScope(t *testing.T) {
	tool := &Tool{RequiredScopes: []string{"vacancies:read", "scores:write"}}

	if !tool.Permits([]string{"vacancies:read", "scores:write", "tools:call"}) {
		t.Error("refused an agent holding every required scope")
	}
	if tool.Permits([]string{"vacancies:read"}) {
		t.Error("admitted an agent missing a scope")
	}
	if tool.Permits(nil) {
		t.Error("admitted an agent holding nothing")
	}
}

func TestToolWithNoScopesIsOpen(t *testing.T) {
	if !(&Tool{RequiredScopes: []string{}}).Permits(nil) {
		t.Error("a tool requiring nothing refused an agent holding nothing")
	}
}

// `required: [[]]` is an array, so a check that only tests the type lets it
// past and the failure surfaces as an unreadable metaschema error later.
func TestSchemaPolicyRejectsNonStringRequiredEntries(t *testing.T) {
	bad := json.RawMessage(`{"type":"object","additionalProperties":false,"required":[[]]}`)
	err := CheckSchemaPolicy(bad)
	if !errors.Is(err, ErrSchemaPolicy) {
		t.Fatalf("got %v, want ErrSchemaPolicy", err)
	}
	if !strings.Contains(err.Error(), "required[0]") {
		t.Errorf("error does not name the offending entry: %v", err)
	}
}
