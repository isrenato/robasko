// Package boardapi adapts a job board that returns a flat JSON list.
//
// The shape it expects:
//
//	{"data": [{"slug","title","company_name","location","remote",
//	           "description","tags","url","created_at"}]}
//
// The host is supplied by configuration, never compiled in. The adapter
// knows the response shape it can parse; it does not know, and does not
// name, whose server answers.
package boardapi

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/isrenato/robasko/internal/sources"
)

// Adapter fetches from one configured board.
type Adapter struct {
	name    string
	baseURL string
	apiKey  string
	client  *http.Client
}

// New returns an adapter for the board at baseURL.
func New(name, baseURL, apiKey string) (*Adapter, error) {
	if name == "" {
		return nil, fmt.Errorf("boardapi: empty source name")
	}
	if baseURL == "" {
		return nil, fmt.Errorf("boardapi: empty base URL for source %q", name)
	}
	return &Adapter{
		name:    name,
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		client:  &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (a *Adapter) Name() string { return a.name }

// payload mirrors the part of the response the adapter uses. Unlisted
// fields are ignored rather than rejected: a board adding a field must not
// break a fetch.
type payload struct {
	Data []struct {
		Slug        string   `json:"slug"`
		Title       string   `json:"title"`
		Company     string   `json:"company_name"`
		Location    string   `json:"location"`
		Remote      bool     `json:"remote"`
		Description string   `json:"description"`
		Tags        []string `json:"tags"`
		URL         string   `json:"url"`
		CreatedAt   int64    `json:"created_at"`
	} `json:"data"`
}

// Fetch returns vacancies matching q.
//
// The board paginates rather than filtering server side, so matching is
// done here over the title, company and tags. Limit 0 means no limit.
func (a *Adapter) Fetch(ctx context.Context, q sources.Query) ([]sources.RawVacancy, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+"/job-board-api", nil)
	if err != nil {
		return nil, fmt.Errorf("boardapi: building request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "robasko/0.1")
	if a.apiKey != "" {
		req.Header.Set("X-API-Key", a.apiKey)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("boardapi: fetching from %s: %w", a.name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("boardapi: %s returned %s: %s",
			a.name, resp.Status, strings.TrimSpace(string(body)))
	}

	var p payload
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return nil, fmt.Errorf("boardapi: decoding the response from %s: %w", a.name, err)
	}

	var out []sources.RawVacancy
	for _, j := range p.Data {
		if !matches(q, j.Title, j.Company, j.Location, j.Tags) {
			continue
		}
		out = append(out, sources.RawVacancy{
			SourceID:    j.Slug,
			URL:         j.URL,
			Title:       strings.TrimSpace(j.Title),
			Company:     strings.TrimSpace(j.Company),
			City:        strings.TrimSpace(j.Location),
			Remote:      j.Remote,
			Tags:        j.Tags,
			PublishedAt: time.Unix(j.CreatedAt, 0).UTC(),
			Text:        plainText(j.Description),
		})
		if q.Limit > 0 && len(out) >= q.Limit {
			break
		}
	}
	return out, nil
}

func matches(q sources.Query, title, company, location string, tags []string) bool {
	if q.What != "" {
		hay := title + " " + company + " " + strings.Join(tags, " ")
		if !containsWord(hay, q.What) {
			return false
		}
	}
	if q.Where != "" && !strings.Contains(strings.ToLower(location), strings.ToLower(q.Where)) {
		return false
	}
	return true
}

// wordRe caches one compiled pattern per term, since Fetch tests the same
// term against every vacancy on the page.
var wordRe sync.Map // string -> *regexp.Regexp

// containsWord reports whether term occurs in hay as a whole word.
//
// Plain substring matching is useless for the short names this domain is
// full of: "go" is inside "Cellagon" and "Google", and a search for a Go
// position returns neither kind of job.
func containsWord(hay, term string) bool {
	term = strings.TrimSpace(term)
	if term == "" {
		return true
	}
	re, ok := wordRe.Load(term)
	if !ok {
		compiled, err := regexp.Compile(`(?i)(^|[^\p{L}\p{N}])` + regexp.QuoteMeta(term) + `($|[^\p{L}\p{N}])`)
		if err != nil {
			return strings.Contains(strings.ToLower(hay), strings.ToLower(term))
		}
		wordRe.Store(term, compiled)
		re = compiled
	}
	return re.(*regexp.Regexp).MatchString(hay)
}

var (
	tagRe   = regexp.MustCompile(`(?s)<[^>]*>`)
	spaceRe = regexp.MustCompile(`[ \t]+`)
	blankRe = regexp.MustCompile(`\n{3,}`)
)

// plainText strips the markup boards wrap descriptions in. The result is
// what gets stored as raw_text and read by requirement extraction, so
// paragraph breaks are kept and everything else is collapsed.
func plainText(s string) string {
	s = strings.ReplaceAll(s, "</p>", "\n\n")
	s = strings.ReplaceAll(s, "<br>", "\n")
	s = strings.ReplaceAll(s, "<br/>", "\n")
	s = strings.ReplaceAll(s, "<br />", "\n")
	s = strings.ReplaceAll(s, "</li>", "\n")
	s = tagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = spaceRe.ReplaceAllString(s, " ")
	s = blankRe.ReplaceAllString(s, "\n\n")

	var lines []string
	for _, l := range strings.Split(s, "\n") {
		lines = append(lines, strings.TrimSpace(l))
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
