package boardapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/isrenato/robasko/internal/sources"
)

// fixture is written by hand rather than recorded, so no third party's
// advertisement or contact details enter the repository.
const fixture = `{"data":[
 {"slug":"a-1","title":"Senior Backend Engineer - Golang","company_name":"Nordwind Logistik GmbH",
  "location":"Berlin","remote":true,"tags":["Software Development","Go"],
  "url":"https://board.example/jobs/a-1","created_at":1760000000,
  "description":"<p>Wir suchen Verst&auml;rkung.</p><ul><li>Mehrj&auml;hrige Erfahrung mit Go</li><li>Idealerweise Kubernetes</li></ul>"},
 {"slug":"a-2","title":"Controller (m/w/d)","company_name":"Cellagon GmbH",
  "location":"Stuttgart","remote":false,"tags":["Controlling"],
  "url":"https://board.example/jobs/a-2","created_at":1760000001,
  "description":"<p>Zahlen und Berichte.</p>"},
 {"slug":"a-3","title":"Google Ads Manager","company_name":"Lumenstadt Technik",
  "location":"Berlin","remote":false,"tags":["Marketing"],
  "url":"https://board.example/jobs/a-3","created_at":1760000002,
  "description":"<p>Kampagnen.</p>"}
]}`

func serve(t *testing.T, body string, status int) *Adapter {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/job-board-api" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	a, err := New("board", srv.URL, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func TestFetchParsesVacancies(t *testing.T) {
	got, err := serve(t, fixture, 200).Fetch(context.Background(), sources.Query{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d vacancies, want 3", len(got))
	}

	v := got[0]
	if v.SourceID != "a-1" || v.Title != "Senior Backend Engineer - Golang" {
		t.Errorf("unexpected vacancy: %+v", v)
	}
	if !v.Remote || v.City != "Berlin" {
		t.Errorf("location fields wrong: %+v", v)
	}
	if v.PublishedAt.IsZero() {
		t.Error("published date not parsed")
	}
}

// "go" is inside "Cellagon" and inside "Google". A search for Go work must
// return neither the controller nor the ads manager - but it must still
// return the job tagged "Go", which is a real occurrence of the word.
func TestWholeWordMatchingIgnoresSubstrings(t *testing.T) {
	a := serve(t, fixture, 200)

	cases := map[string][]string{
		"go":      {"Senior Backend Engineer - Golang"}, // via the "Go" tag
		"Go":      {"Senior Backend Engineer - Golang"},
		"golang":  {"Senior Backend Engineer - Golang"},
		"backend": {"Senior Backend Engineer - Golang"},
		"gon":     {}, // inside "Cellagon", not a word
		"oogle":   {}, // inside "Google", not a word
	}

	for term, want := range cases {
		got, err := a.Fetch(context.Background(), sources.Query{What: term})
		if err != nil {
			t.Fatalf("Fetch(%q): %v", term, err)
		}
		var titles []string
		for _, v := range got {
			titles = append(titles, v.Title)
		}
		if len(titles) != len(want) {
			t.Errorf("%q matched %v, want %v", term, titles, want)
			continue
		}
		for i := range want {
			if titles[i] != want[i] {
				t.Errorf("%q matched %v, want %v", term, titles, want)
				break
			}
		}
	}
}

func TestFiltersByLocation(t *testing.T) {
	got, _ := serve(t, fixture, 200).Fetch(context.Background(), sources.Query{Where: "berlin"})
	if len(got) != 2 {
		t.Errorf("got %d, want 2", len(got))
	}
}

func TestLimitStopsEarly(t *testing.T) {
	got, _ := serve(t, fixture, 200).Fetch(context.Background(), sources.Query{Limit: 2})
	if len(got) != 2 {
		t.Errorf("got %d, want 2", len(got))
	}
}

// raw_text is read by requirement extraction, so the markup has to go and
// the entities have to be decoded, while the list structure stays legible.
func TestDescriptionIsReducedToPlainText(t *testing.T) {
	got, _ := serve(t, fixture, 200).Fetch(context.Background(), sources.Query{What: "golang"})
	text := got[0].Text

	if strings.Contains(text, "<") {
		t.Errorf("markup survived: %q", text)
	}
	if !strings.Contains(text, "Verstärkung") {
		t.Errorf("entity not decoded: %q", text)
	}
	if !strings.Contains(text, "Idealerweise Kubernetes") {
		t.Errorf("list item lost: %q", text)
	}
}

func TestNonOKStatusIsReported(t *testing.T) {
	_, err := serve(t, "forbidden", 403).Fetch(context.Background(), sources.Query{})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("got %v, want the status in the error", err)
	}
}

func TestMalformedJSONIsReported(t *testing.T) {
	_, err := serve(t, "{not json", 200).Fetch(context.Background(), sources.Query{})
	if err == nil || !strings.Contains(err.Error(), "decoding") {
		t.Errorf("got %v, want a decode error", err)
	}
}

func TestNewRejectsMissingConfiguration(t *testing.T) {
	if _, err := New("", "https://a.example", ""); err == nil {
		t.Error("expected an empty name to be rejected")
	}
	if _, err := New("board", "", ""); err == nil {
		t.Error("expected an empty base URL to be rejected")
	}
}
