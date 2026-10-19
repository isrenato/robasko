package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func fakeWith(t *testing.T, reply string) *Fake {
	t.Helper()
	f := NewFake()
	f.Reply("only", json.RawMessage(reply))
	return f
}

func TestExtractParsesRequirements(t *testing.T) {
	f := fakeWith(t, `{"requirements":[
		{"text":"Mehrjährige Erfahrung mit Go","kind":"must","category":"language"},
		{"text":"Idealerweise Kubernetes","kind":"nice","category":"cloud"}
	]}`)

	got, resp, err := ExtractRequirements(context.Background(), f, "Wir suchen eine Entwicklerin.")
	if err != nil {
		t.Fatalf("ExtractRequirements: %v", err)
	}
	if len(got.Requirements) != 2 {
		t.Fatalf("got %d requirements, want 2", len(got.Requirements))
	}
	if got.Requirements[0].Kind != KindMust || got.Requirements[1].Kind != KindNice {
		t.Errorf("kinds not read: %+v", got.Requirements)
	}
	if resp.InputTokens == 0 {
		t.Error("usage not reported")
	}
}

// The advertisement must come after the frozen system prompt, or the cached
// prefix changes on every call and the cache never hits.
func TestAdvertisementGoesAfterTheFrozenSystemPrompt(t *testing.T) {
	f := fakeWith(t, `{"requirements":[]}`)

	for _, ad := range []string{"first advertisement", "second advertisement"} {
		if _, _, err := ExtractRequirements(context.Background(), f, ad); err != nil {
			t.Fatalf("ExtractRequirements: %v", err)
		}
	}
	if len(f.Calls) != 2 {
		t.Fatalf("got %d calls", len(f.Calls))
	}
	if f.Calls[0].System != f.Calls[1].System {
		t.Error("the system prompt differed between advertisements")
	}
	if f.Calls[0].System == "" {
		t.Error("no system prompt was sent")
	}
	if !strings.Contains(f.Calls[1].Prompt, "second advertisement") {
		t.Errorf("the advertisement did not reach the prompt: %q", f.Calls[1].Prompt)
	}
}

// The schema is what makes the reply usable, so it has to say what the
// registry's policy says: an object that rejects unknown properties.
func TestSchemaIsClosed(t *testing.T) {
	s := ExtractSchema()
	if s["additionalProperties"] != false {
		t.Error("the top level accepts unknown properties")
	}

	items := s["properties"].(map[string]any)["requirements"].(map[string]any)["items"].(map[string]any)
	if items["additionalProperties"] != false {
		t.Error("a requirement accepts unknown properties")
	}
	required, _ := items["required"].([]string)
	if len(required) != 3 {
		t.Errorf("a requirement requires %v, want text, kind and category", required)
	}

	props := items["properties"].(map[string]any)
	kinds := props["kind"].(map[string]any)["enum"].([]string)
	if len(kinds) != 2 {
		t.Errorf("kind allows %v, want exactly must and nice", kinds)
	}
	// An open category set would let the model invent one nothing
	// aggregates on.
	if cats := props["category"].(map[string]any)["enum"].([]string); len(cats) != len(Categories) {
		t.Errorf("category enum has %d entries, want %d", len(cats), len(Categories))
	}
}

func TestEmptyAdvertisementIsRejectedWithoutCallingTheModel(t *testing.T) {
	f := fakeWith(t, `{"requirements":[]}`)

	if _, _, err := ExtractRequirements(context.Background(), f, "   \n "); err == nil {
		t.Error("an empty advertisement was sent to the model")
	}
	if len(f.Calls) != 0 {
		t.Errorf("the model was called %d times", len(f.Calls))
	}
}

// A reply that satisfies the schema but not this struct means the two have
// drifted; that is worth an error rather than a zero value.
func TestMalformedReplyIsReported(t *testing.T) {
	for name, reply := range map[string]string{
		"empty text":   `{"requirements":[{"text":"  ","kind":"must","category":"language"}]}`,
		"unknown kind": `{"requirements":[{"text":"Go","kind":"maybe","category":"language"}]}`,
		"not an object": `["Go"]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := ExtractRequirements(context.Background(), fakeWith(t, reply), "ad"); err == nil {
				t.Error("accepted")
			}
		})
	}
}

func TestExtractPropagatesAModelError(t *testing.T) {
	f := NewFake()
	f.Err = ErrRefused

	if _, _, err := ExtractRequirements(context.Background(), f, "ad"); !errors.Is(err, ErrRefused) {
		t.Errorf("got %v, want ErrRefused", err)
	}
}

// A fixture keyed to one exchange must not answer a different one: a stale
// recording should miss rather than quietly reply to the wrong question.
func TestFixturesAreKeyedToTheExchange(t *testing.T) {
	f := NewFake()
	f.ReplyTo(extractSystem, "Advertisement:\n\nfirst", json.RawMessage(`{"requirements":[]}`))
	f.ReplyTo(extractSystem, "Advertisement:\n\nsecond", json.RawMessage(`{"requirements":[{"text":"Go","kind":"must","category":"language"}]}`))

	first, _, err := ExtractRequirements(context.Background(), f, "first")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if len(first.Requirements) != 0 {
		t.Errorf("the wrong fixture answered: %+v", first)
	}

	second, _, err := ExtractRequirements(context.Background(), f, "second")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if len(second.Requirements) != 1 {
		t.Errorf("the wrong fixture answered: %+v", second)
	}

	if _, _, err := ExtractRequirements(context.Background(), f, "unrecorded"); err == nil {
		t.Error("an unrecorded exchange was answered anyway")
	}
}

func TestNewRequiresAKey(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("a client was built without a key")
	}
}

// The system prompt is the only defence against the two failures that made
// the alias-matching stand-in unusable.
func TestSystemPromptAddressesNegationAndBoilerplate(t *testing.T) {
	// Whitespace is normalised first: the prompt is hard-wrapped, so a
	// phrase can straddle a line break and a literal substring check
	// would fail on formatting rather than on content.
	flat := strings.Join(strings.Fields(extractSystem), " ")

	for _, want := range []string{"nicht erforderlich", "footer", "verbatim"} {
		if !strings.Contains(flat, want) {
			t.Errorf("the system prompt does not mention %q", want)
		}
	}
}
