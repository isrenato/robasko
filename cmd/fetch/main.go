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
)

func main() {
	var (
		envFile = flag.String("env", "env.lock", "file holding SOURCE_<n>_* settings")
		what    = flag.String("what", "", "match against title, company and tags")
		where   = flag.String("where", "", "match against the location")
		limit   = flag.Int("limit", 10, "stop after this many vacancies, 0 for all")
		full    = flag.Bool("full", false, "print the whole advertisement text")
	)
	flag.Parse()

	if err := run(*envFile, sources.Query{What: *what, Where: *where, Limit: *limit}, *full); err != nil {
		fmt.Fprintf(os.Stderr, "fetch: %v\n", err)
		os.Exit(1)
	}
}

func run(envFile string, q sources.Query, full bool) error {
	look, err := config.LoadFile(envFile)
	if err != nil {
		return err
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

		fmt.Printf("source %q: %d vacancies\n\n", adapter.Name(), len(vacancies))
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
			if full {
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
