package buildcfg

import "testing"

// The images have to be able to build what the module requires. When this
// fails, either `go mod tidy` raised the directive under a newer local
// toolchain or an image tag was left behind; align them rather than
// lowering the directive, which the next tidy would undo.
func TestImagesCanBuildWhatTheModuleRequires(t *testing.T) {
	required, err := ModuleRequires("../../go.mod")
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}

	for _, file := range []string{"../../Dockerfile", "../../docker-compose.yml"} {
		images, err := ImageVersions(file)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if len(images) == 0 {
			t.Errorf("%s references no golang image; has it been renamed?", file)
		}
		for tag, got := range images {
			if !got.AtLeast(required) {
				t.Errorf("%s pins golang:%s (Go %s) but go.mod requires %s",
					file, tag, got, required)
			}
		}
	}
}

func TestVersionComparison(t *testing.T) {
	cases := []struct {
		have, want string
		ok         bool
	}{
		{"1.25", "1.25", true},
		{"1.25.3", "1.25", true},
		{"1.26", "1.25", true},
		{"1.23", "1.25", false},
		{"2.0", "1.25", true},
	}
	for _, c := range cases {
		have, _ := ParseVersion(c.have)
		want, _ := ParseVersion(c.want)
		if got := have.AtLeast(want); got != c.ok {
			t.Errorf("%s AtLeast %s = %v, want %v", c.have, c.want, got, c.ok)
		}
	}
}

func TestParseVersionRejectsNonsense(t *testing.T) {
	if _, err := ParseVersion("alpine"); err == nil {
		t.Error("expected an error")
	}
}
