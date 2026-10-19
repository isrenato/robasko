// Command score runs the analysis half of the pipeline over vacancies
// already stored: it reads each advertisement, has the model name the
// requirements, maps them onto the inventory and computes the score.
//
// Fetching is a separate command on purpose. Fetching is cheap and
// repeatable; this spends money per vacancy, and the two should not be one
// flag apart.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/isrenato/robasko/internal/config"
	"github.com/isrenato/robasko/internal/inventory"
	"github.com/isrenato/robasko/internal/llm"
	"github.com/isrenato/robasko/internal/scoring"
	"github.com/isrenato/robasko/internal/store"
)

func main() {
	var (
		envFile = flag.String("env", "env.lock", "file holding the settings")
		invPath = flag.String("inventory", "inventory.yaml", "inventory to score against")
		limit   = flag.Int("limit", 5, "how many unscored vacancies to work through")
		dry     = flag.Bool("dry-run", false, "print the result without storing it")
	)
	flag.Parse()

	if err := run(*envFile, *invPath, *limit, *dry); err != nil {
		fmt.Fprintf(os.Stderr, "score: %v\n", err)
		os.Exit(1)
	}
}

func run(envFile, invPath string, limit int, dry bool) error {
	look, err := config.LoadFile(envFile)
	if err != nil {
		return err
	}

	inv, err := inventory.Load(invPath)
	if err != nil {
		return err
	}

	model, err := llm.New(llm.Options{
		APIKey:      look("ANTHROPIC_API_KEY"),
		BaseURL:     look("ANTHROPIC_BASE_URL"),
		Model:       look("ANTHROPIC_MODEL"),
		WorkspaceID: look("ANTHROPIC_WORKSPACE_ID"),
	})
	if err != nil {
		return err
	}

	dsn := look("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	st, err := store.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer st.Close()

	pending, err := st.Unscored(ctx, limit)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		fmt.Println("nothing to score: every stored vacancy already has one")
		return nil
	}

	fmt.Printf("scoring %d vacancies against %s (%d entries, digest %s)\n\n",
		len(pending), invPath, len(inv.Entries), inv.Digest())

	var inTok, outTok, cached int64
	for i, v := range pending {
		fmt.Printf("%d. %s\n   %s | %s | %s\n", i+1, v.Title, v.Company, v.City, v.Remote)

		scored, usage, err := analyse(ctx, model, inv, v)
		inTok += usage.InputTokens
		outTok += usage.OutputTokens
		cached += usage.CacheRead
		if err != nil {
			// One advertisement failing is not a reason to abandon the
			// rest: a refusal or a malformed reply is specific to its text.
			fmt.Printf("   SKIPPED: %v\n\n", err)
			continue
		}

		printResult(scored)
		if !dry {
			if err := st.SaveScore(ctx, scored); err != nil {
				return err
			}
		}
		fmt.Println()
	}

	fmt.Printf("tokens: %d in, %d out, %d read from cache\n", inTok, outTok, cached)
	if dry {
		fmt.Println("dry run: nothing was stored")
	}
	return nil
}

// analyse is the pipeline for one advertisement: extract, map, compute.
func analyse(ctx context.Context, c llm.Client, inv *inventory.Inventory, v store.StoredVacancy) (store.Scored, llm.Response, error) {
	var usage llm.Response

	extracted, resp, err := llm.ExtractRequirements(ctx, c, v.Text)
	if resp != nil {
		usage = *resp
	}
	if err != nil {
		return store.Scored{}, usage, fmt.Errorf("extracting: %w", err)
	}

	mapped, resp, err := llm.MapRequirements(ctx, c, extracted.Requirements, inv)
	if resp != nil {
		usage.InputTokens += resp.InputTokens
		usage.OutputTokens += resp.OutputTokens
		usage.CacheRead += resp.CacheRead
	}
	if err != nil {
		return store.Scored{}, usage, fmt.Errorf("mapping: %w", err)
	}

	categories := make([]string, len(extracted.Requirements))
	for i, r := range extracted.Requirements {
		categories[i] = r.Category
	}

	// The commute cap needs a distance this command does not have, so it
	// is left off rather than guessed: a cap applied on a guess is worse
	// than one not applied.
	result := scoring.Compute(mapped, scoring.Options{})

	return store.Scored{
		VacancyID:       v.ID,
		Requirements:    mapped,
		Categories:      categories,
		Result:          result,
		Model:           usage.Model,
		InventoryDigest: inv.Digest(),
	}, usage, nil
}

func printResult(s store.Scored) {
	r := s.Result
	fmt.Printf("   SCORE %.1f   must %.2f (%d)   nice %.2f (%d)",
		r.Score, r.MustCoverage, r.MustCount, r.NiceCoverage, r.NiceCount)
	if r.CappedReason != "" {
		fmt.Printf("   capped: %s", r.CappedReason)
	}
	fmt.Println()

	if len(s.Requirements) == 0 {
		fmt.Println("     the model read no requirements from this advertisement")
		return
	}
	fmt.Printf("     %-5s %5s  %-22s %s\n", "kind", "cover", "inventory", "requirement")
	for _, req := range s.Requirements {
		ids := strings.Join(req.IDs, ",")
		if ids == "" {
			ids = "-"
		}
		fmt.Printf("     %-5s %5.2f  %-22s %s\n",
			req.Kind, req.Coverage, truncate(ids, 22), truncate(req.Text, 58))
	}
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
