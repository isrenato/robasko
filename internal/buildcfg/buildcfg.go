// Package buildcfg reads the Go version the module requires and the
// versions the container images provide.
//
// These drift apart silently. The module's `go` directive is rewritten by
// `go mod tidy` to match whichever toolchain happens to be installed, while
// the image tags are literals nobody updates. The result compiles on the
// developer's machine and fails inside the container minutes later, which
// is the worst place to find out.
package buildcfg

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Version is a Go release, compared by major and minor only: a module
// requiring 1.25 is satisfied by any 1.25.x.
type Version struct{ Major, Minor int }

func (v Version) String() string { return fmt.Sprintf("%d.%d", v.Major, v.Minor) }

// AtLeast reports whether v is new enough to satisfy want.
func (v Version) AtLeast(want Version) bool {
	if v.Major != want.Major {
		return v.Major > want.Major
	}
	return v.Minor >= want.Minor
}

var versionRe = regexp.MustCompile(`(\d+)\.(\d+)`)

// ParseVersion reads the major and minor from a version string, ignoring
// any patch component and surrounding text.
func ParseVersion(s string) (Version, error) {
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("no version in %q", s)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return Version{Major: major, Minor: minor}, nil
}

var (
	goDirectiveRe = regexp.MustCompile(`(?m)^go\s+(\S+)`)
	// The numeric part is matched explicitly. A lazy catch-all stops at the
	// first word boundary, which inside "1.25-alpine" is after the "1".
	golangImageRe = regexp.MustCompile(`golang:(\d+(?:\.\d+)*)`)
)

// ModuleRequires returns the version the module's go directive demands.
func ModuleRequires(goModPath string) (Version, error) {
	raw, err := os.ReadFile(goModPath)
	if err != nil {
		return Version{}, err
	}
	m := goDirectiveRe.FindSubmatch(raw)
	if m == nil {
		return Version{}, fmt.Errorf("%s has no go directive", goModPath)
	}
	return ParseVersion(string(m[1]))
}

// ImageVersions returns the Go version of every golang: image referenced in
// a file, keyed by the tag it was written as.
func ImageVersions(path string) (map[string]Version, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]Version{}
	for _, m := range golangImageRe.FindAllStringSubmatch(string(raw), -1) {
		tag := strings.TrimSpace(m[1])
		v, err := ParseVersion(tag)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out[tag] = v
	}
	return out, nil
}
