// Package llm wraps the one model call the agents need: send text, get
// back JSON matching a schema.
//
// Structured outputs rather than a tool-calling loop. Extraction is a
// single request with a known answer shape, and output_config.format makes
// the model's reply conform to that shape; a tool loop would add turns,
// tokens and failure modes for nothing. Forced tool choice is rejected on
// the current models anyway.
//
// The interface exists so tests can replay recorded exchanges. CI never
// calls the API.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// DefaultModel is the model used when none is configured.
const DefaultModel = "claude-opus-5-5"

// Effort levels, in the order they cost.
const (
	EffortLow    = "low"
	EffortMedium = "medium"
	EffortHigh   = "high"
)

// Request is one structured-output call.
type Request struct {
	// System is held stable across calls so the prompt cache can keep its
	// prefix: tools, then system, then messages. Anything volatile belongs
	// in Prompt, which comes last.
	System string
	Prompt string
	// Schema is the JSON Schema the reply must satisfy.
	Schema map[string]any
	// Effort defaults to medium. The current Opus defaults to medium too,
	// but it is set explicitly because that default differs by model.
	Effort    string
	MaxTokens int64
}

// Response is the model's reply and what it cost.
type Response struct {
	JSON         json.RawMessage
	Model        string
	InputTokens  int64
	OutputTokens int64
	CacheRead    int64
	CacheWrite   int64
	StopReason   string
	Duration     time.Duration
}

// Client is the one call the agents need.
type Client interface {
	Structured(ctx context.Context, req Request) (*Response, error)
}

// Errors a caller is expected to distinguish.
var (
	// ErrRetryable covers rate limiting and server faults: the same
	// request may succeed later.
	ErrRetryable = errors.New("retryable model error")
	// ErrRefused reports a request the model declined. Retrying it
	// unchanged will not help.
	ErrRefused = errors.New("model refused the request")
	// ErrBadRequest covers a malformed request or an unknown model.
	ErrBadRequest = errors.New("model rejected the request")
)

// Anthropic calls the real API.
type Anthropic struct {
	client anthropic.Client
	model  string
}

// Options configure the live client.
type Options struct {
	APIKey  string
	BaseURL string
	Model   string
	// WorkspaceID is required when the key is not itself scoped to a
	// workspace; such a key is rejected with a 400 naming this header.
	WorkspaceID string
}

// New returns a live client. The key is required; the base URL is honoured
// when set, so the client can be pointed at a proxy or a recorder without
// a rebuild.
func New(opts Options) (*Anthropic, error) {
	if strings.TrimSpace(opts.APIKey) == "" {
		return nil, fmt.Errorf("llm: no API key")
	}
	clientOpts := []option.RequestOption{option.WithAPIKey(opts.APIKey)}
	if opts.BaseURL != "" {
		clientOpts = append(clientOpts, option.WithBaseURL(opts.BaseURL))
	}
	if opts.WorkspaceID != "" {
		clientOpts = append(clientOpts, option.WithHeader("anthropic-workspace-id", opts.WorkspaceID))
	}

	model := opts.Model
	if model == "" {
		model = DefaultModel
	}
	return &Anthropic{client: anthropic.NewClient(clientOpts...), model: model}, nil
}

// maxAttempts bounds the retry loop for rate limits and server faults.
const maxAttempts = 4

// Structured sends one request and returns the JSON reply.
func (a *Anthropic) Structured(ctx context.Context, req Request) (*Response, error) {
	if len(req.Schema) == 0 {
		return nil, fmt.Errorf("llm: no schema")
	}
	effort := req.Effort
	if effort == "" {
		effort = EffortMedium
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		// Enough headroom for a long requirement list without risking the
		// non-streaming HTTP timeout.
		maxTokens = 16000
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(a.model),
		MaxTokens: maxTokens,
		OutputConfig: anthropic.OutputConfigParam{
			Effort: anthropic.OutputConfigEffort(effort),
			Format: anthropic.JSONOutputFormatParam{Schema: req.Schema},
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(req.Prompt)),
		},
	}
	if req.System != "" {
		params.System = []anthropic.TextBlockParam{{
			Text: req.System,
			// The system prompt is frozen, so caching it pays on every
			// advertisement after the first.
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}}
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		started := time.Now()
		msg, err := a.client.Messages.New(ctx, params)
		if err == nil {
			return decode(msg, time.Since(started))
		}

		lastErr = classify(err)
		if !errors.Is(lastErr, ErrRetryable) || attempt == maxAttempts {
			return nil, lastErr
		}

		wait := time.Duration(1<<attempt) * time.Second
		slog.Warn("llm: retrying", "attempt", attempt, "wait", wait, "error", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	return nil, lastErr
}

func decode(msg *anthropic.Message, took time.Duration) (*Response, error) {
	out := &Response{
		Model:        string(msg.Model),
		InputTokens:  msg.Usage.InputTokens,
		OutputTokens: msg.Usage.OutputTokens,
		CacheRead:    msg.Usage.CacheReadInputTokens,
		CacheWrite:   msg.Usage.CacheCreationInputTokens,
		StopReason:   string(msg.StopReason),
		Duration:     took,
	}

	// Checked before the content is read: a refusal returns HTTP 200 with
	// no usable body.
	if msg.StopReason == anthropic.StopReasonRefusal {
		return out, fmt.Errorf("%w: %s", ErrRefused, msg.StopDetails.Explanation)
	}

	for _, block := range msg.Content {
		if text, ok := block.AsAny().(anthropic.TextBlock); ok {
			out.JSON = json.RawMessage(strings.TrimSpace(text.Text))
			break
		}
	}
	if len(out.JSON) == 0 {
		return out, fmt.Errorf("llm: the reply carried no text block (stop reason %q)", msg.StopReason)
	}
	if !json.Valid(out.JSON) {
		return out, fmt.Errorf("llm: the reply is not valid JSON despite the schema")
	}
	return out, nil
}

// classify turns a transport or API error into one of the sentinels, so a
// caller can tell a rate limit from a malformed request without matching
// on message text.
func classify(err error) error {
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		// A connection failure is worth another attempt.
		return fmt.Errorf("%w: %v", ErrRetryable, err)
	}
	switch {
	case apiErr.StatusCode == 429, apiErr.StatusCode >= 500:
		return fmt.Errorf("%w: %v", ErrRetryable, err)
	case apiErr.StatusCode == 400, apiErr.StatusCode == 404, apiErr.StatusCode == 422:
		return fmt.Errorf("%w: %v", ErrBadRequest, err)
	default:
		return err
	}
}
