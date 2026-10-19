// Package config reads runtime configuration from the environment.
//
// Sources are numbered rather than named, and no base URL has a default.
// Which providers this runs against is deployment configuration, so it
// lives beside the credentials and not in the source tree.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// maxSources bounds the scan for SOURCE_<n>_* slots.
const maxSources = 16

// Source is one configured job board.
type Source struct {
	Name    string // stored in the vacancies.source column; must stay stable
	BaseURL string // up to but not including the resource path
	APIKey  string
	APIID   string // only some providers use a two-part credential
}

// Sources reads every configured SOURCE_<n>_* slot, in order.
//
// A slot is considered present when it sets a name or a base URL; a slot
// that sets one without the other is a configuration error rather than
// something to guess at.
func Sources(look func(string) string) ([]Source, error) {
	var out []Source
	for n := 1; n <= maxSources; n++ {
		p := "SOURCE_" + strconv.Itoa(n) + "_"
		name := strings.TrimSpace(look(p + "NAME"))
		base := strings.TrimRight(strings.TrimSpace(look(p+"BASE_URL")), "/")

		if name == "" && base == "" {
			continue
		}
		if name == "" {
			return nil, fmt.Errorf("%sBASE_URL is set but %sNAME is not", p, p)
		}
		if base == "" {
			return nil, fmt.Errorf("%sNAME is set but %sBASE_URL is not", p, p)
		}
		if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
			return nil, fmt.Errorf("%sBASE_URL must start with http:// or https://", p)
		}
		for _, prior := range out {
			if prior.Name == name {
				return nil, fmt.Errorf("source name %q is used twice", name)
			}
		}
		out = append(out, Source{
			Name:    name,
			BaseURL: base,
			APIKey:  strings.TrimSpace(look(p + "API_KEY")),
			APIID:   strings.TrimSpace(look(p + "API_ID")),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no sources configured: set SOURCE_1_NAME and SOURCE_1_BASE_URL")
	}
	return out, nil
}

// FromEnv reads the sources from the process environment.
func FromEnv() ([]Source, error) { return Sources(os.Getenv) }

// LoadFile reads KEY=VALUE lines into a lookup function, for a local env
// file that is not exported into the process environment.
func LoadFile(path string) (func(string) string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	vals := map[string]string{}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s line %d: expected KEY=VALUE", path, i+1)
		}
		vals[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return func(k string) string {
		if v, ok := vals[k]; ok {
			return v
		}
		return os.Getenv(k)
	}, nil
}
