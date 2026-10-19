package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Fake replays recorded replies instead of calling the API, so the test
// suite runs without network and without spending anything.
//
// Fixtures are keyed by a hash of the system prompt and the user prompt
// together: a change to either is a different exchange, and a stale
// fixture should miss rather than quietly answer the wrong question.
type Fake struct {
	mu      sync.Mutex
	replies map[string]json.RawMessage
	Calls   []Request
	// Err, when set, is returned instead of a reply.
	Err error
}

// NewFake returns a fake with no fixtures loaded.
func NewFake() *Fake { return &Fake{replies: map[string]json.RawMessage{}} }

// LoadFixtures reads every .json file in dir, keyed by filename stem.
func LoadFixtures(dir string) (*Fake, error) {
	f := NewFake()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("llm: reading fixtures: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		if !json.Valid(raw) {
			return nil, fmt.Errorf("llm: fixture %s is not valid JSON", e.Name())
		}
		f.replies[strings.TrimSuffix(e.Name(), ".json")] = raw
	}
	return f, nil
}

// Reply registers a canned answer under a name.
func (f *Fake) Reply(name string, reply json.RawMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies[name] = reply
}

// ReplyTo registers a canned answer for one exact exchange.
func (f *Fake) ReplyTo(system, prompt string, reply json.RawMessage) {
	f.Reply(FixtureKey(system, prompt), reply)
}

// FixtureKey is the name a recorded exchange is stored under.
func FixtureKey(system, prompt string) string {
	sum := sha256.Sum256([]byte(system + "\x00" + prompt))
	return hex.EncodeToString(sum[:])[:16]
}

// Structured returns the reply registered for this exchange.
func (f *Fake) Structured(_ context.Context, req Request) (*Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, req)

	if f.Err != nil {
		return nil, f.Err
	}

	key := FixtureKey(req.System, req.Prompt)
	reply, ok := f.replies[key]
	if !ok {
		// A single unnamed reply stands in for any exchange, which keeps
		// a test that only cares about the shape from needing a hash.
		if only, single := f.single(); single {
			reply = only
		} else {
			return nil, fmt.Errorf("llm: no fixture for exchange %s", key)
		}
	}
	return &Response{
		JSON: reply, Model: "fake", StopReason: "end_turn",
		InputTokens: int64(len(req.Prompt) / 4), OutputTokens: int64(len(reply) / 4),
	}, nil
}

func (f *Fake) single() (json.RawMessage, bool) {
	if len(f.replies) != 1 {
		return nil, false
	}
	for _, v := range f.replies {
		return v, true
	}
	return nil, false
}
