// Command fetch retrieves vacancies from a configured source and prints
// them. It is the smallest end-to-end path through the system: no database,
// no queue, no model.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/isrenato/robasko/internal/config"
	"github.com/isrenato/robasko/internal/sources"
	"github.com/isrenato/robasko/internal/sources/boardapi"
	"github.com/isrenato/robasko/internal/store"
)

func main() {
	var (
		envFile = flag.String("env", "env.lock", "file holding SOURCE_<n>_* settings")
		what    = flag.String("what", "", "match against title, company and tags")
		where   = flag.String("where", "", "match against the location")
		limit   = flag.Int("limit", 10, "stop after this many vacancies, 0 for all")
		full    = flag.Bool("full", false, "print the whole advertisement text")
		save    = flag.Bool("save", false, "store the vacancies, skipping ones already held")
		migrate = flag.Bool("migrate", false, "apply pending migrations and exit")
	)
	flag.Parse()

	opts := options{full: *full, save: *save, migrate: *migrate}
	if err := run(*envFile, sources.Query{What: *what, Where: *where, Limit: *limit}, opts); err != nil {
		fmt.Fprintf(os.Stderr, "fetch: %v\n", err)
		os.Exit(1)
	}
}

type options struct {
	full    bool
	save    bool
	migrate bool
}

// openStore connects using DATABASE_URL from the env file or the process
// environment, applies pending migrations and loads the vocabulary.
func openStore(look func(string) string) (*store.Store, error) {
	dsn := look("DATABASE_URL")
	if dsn == "" {
		return nil, fmt.Errorf("DATABASE_URL is not set; add it to the env file")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.Open(ctx, dsn)
	if err != nil {
		return nil, err
	}

	applied, err := st.Migrate(ctx)
	if err != nil {
		st.Close()
		return nil, err
	}
	for _, name := range applied {
		fmt.Printf("migration applied: %s\n", name)
	}

	if _, err := st.LoadTerms(ctx); err != nil {
		st.Close()
		return nil, err
	}
	if stale, err := st.StaleFingerprints(ctx); err == nil && stale > 0 {
		fmt.Printf("warning: %d stored vacancies were fingerprinted under a "+
			"different vocabulary and will not deduplicate\n", stale)
	}
	return st, nil
}

func run(envFile string, q sources.Query, opts options) error {
	look, err := config.LoadFile(envFile)
	if err != nil {
		return err
	}

	if opts.migrate {
		st, err := openStore(look)
		if err != nil {
			return err
		}
		defer st.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		v, c, err := st.Counts(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("schema up to date: %d vacancies, %d companies\n", v, c)
		return nil
	}

	var st *store.Store
	if opts.save {
		st, err = openStore(look)
		if err != nil {
			return err
		}
		defer st.Close()
	}

	configured, err := config.Sources(look)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for _, c := range configured {
		adapter, err := boardapi.New(c.Name, c.BaseURL, c.APIKey)
		if err != nil {
			return err
		}

		vacancies, err := adapter.Fetch(ctx, q)
		if err != nil {
			return err
		}

		fmt.Printf("source %q: %d vacancies\n", adapter.Name(), len(vacancies))

		if st != nil {
			batch := make([]store.Vacancy, 0, len(vacancies))
			for _, v := range vacancies {
				remote := "onsite"
				if v.Remote {
					remote = "remote"
				}
				batch = append(batch, store.Vacancy{
					Source: adapter.Name(), SourceID: v.SourceID, URL: v.URL,
					Title: v.Title, Company: v.Company, City: v.City,
					Remote: remote, PublishedAt: v.PublishedAt, Text: v.Text,
				})
			}
			res, err := st.Save(ctx, batch)
			if err != nil {
				return err
			}
			total, companies, err := st.Counts(ctx)
			if err != nil {
				return err
			}
			fmt.Printf("stored: %d new, %d already held   (total %d vacancies, %d companies)\n",
				res.New, res.Duplicate, total, companies)
		}
		fmt.Println()
		for i, v := range vacancies {
			remote := "onsite"
			if v.Remote {
				remote = "remote"
			}
			fmt.Printf("%2d. %s\n", i+1, v.Title)
			fmt.Printf("    %s | %s | %s | %s\n",
				v.Company, v.City, remote, v.PublishedAt.Format("2006-01-02"))
			if len(v.Tags) > 0 {
				fmt.Printf("    tags: %s\n", strings.Join(v.Tags, ", "))
			}
			fmt.Printf("    %s\n", v.URL)
			if opts.full {
				fmt.Printf("\n%s\n", v.Text)
			} else {
				fmt.Printf("    %s\n", excerpt(v.Text, 150))
			}
			fmt.Println()
		}
	}
	return nil
}

func excerpt(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
