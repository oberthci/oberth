package identityname

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestNameBindsEveryTupleField(t *testing.T) {
	base := Key{Repository: "forge/org/repo", File: ".oberth/release.yaml", Step: "publish"}
	keys := []Key{base, {Repository: "forge/org/other", File: base.File, Step: base.Step},
		{Repository: base.Repository, File: ".oberth/other.yaml", Step: base.Step},
		{Repository: base.Repository, File: base.File, Step: "other"}}
	seen := make(map[string]bool)
	for _, key := range keys {
		name, err := Name(key, Step)
		again, _ := Name(key, Step)
		if err != nil || name != again || seen[name] {
			t.Fatalf("name %q: %v", name, err)
		}
		seen[name] = true
	}
}

func TestNameDistinguishesInputsWithTheSameReadablePart(t *testing.T) {
	base := Key{Repository: "github/a-b/c", File: "ci.yaml", Step: "a-b"}
	cases := []struct {
		name          string
		kinds         []Kind
		first, second Key
	}{
		{"pipeline", []Kind{Pipeline, PipelineCI}, Key{Repository: base.Repository}, Key{Repository: "github/a/b-c"}},
		{"step_repository", []Kind{Step, StepCI}, base, Key{Repository: "github/a/b-c", File: base.File, Step: base.Step}},
		{"step_file", []Kind{Step, StepCI}, base, Key{Repository: base.Repository, File: "ci-yaml", Step: base.Step}},
		{"step_separator", []Kind{Step, StepCI}, base, Key{Repository: base.Repository, File: base.File, Step: "a_b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, kind := range tc.kinds {
				a, err := Name(tc.first, kind)
				if err != nil {
					t.Fatal(err)
				}
				b, err := Name(tc.second, kind)
				if err != nil {
					t.Fatal(err)
				}
				readableA := a[:strings.LastIndexByte(a, '-')]
				readableB := b[:strings.LastIndexByte(b, '-')]
				if readableA != readableB || a == b {
					t.Fatalf("%s: names %q and %q must share readable text and differ in hash", kind, a, b)
				}
			}
		})
	}
}

func TestLongestPrefixesLeaveNonEmptyReadablePart(t *testing.T) {
	for _, tc := range []struct {
		kind   Kind
		prefix string
	}{
		{PipelineCI, "oberth-argo-ci-"},
		{StepCI, "oberth-step-ci-"},
	} {
		key := Key{Repository: strings.Repeat("a", 100) + "/org/repo"}
		if tc.kind == StepCI {
			key.File, key.Step = ".oberth/release.yaml", "publish"
		}
		name, err := Name(key, tc.kind)
		if err != nil {
			t.Fatal(err)
		}
		readable := strings.TrimPrefix(name[:strings.LastIndexByte(name, '-')], tc.prefix)
		if len(name) != 63 || readable != strings.Repeat("a", 27) {
			t.Fatalf("%s: name %q leaves readable part %q", tc.kind, name, readable)
		}
	}
}

func TestNameIsBoundedDNSLabelWithoutSanitizationAliases(t *testing.T) {
	label := regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	for _, kind := range []Kind{Pipeline, PipelineCI, Step, StepCI} {
		seen := make(map[string]bool)
		for _, repo := range []string{"A_B/o/r", "a-b/o/r", "é/o/r", "---/---/---", "forge/org/" + strings.Repeat("long-", 100) + "a", "forge/org/" + strings.Repeat("long-", 100) + "b"} {
			key := Key{Repository: repo}
			if kind == Step || kind == StepCI {
				key.File = ".oberth/release.yaml"
				key.Step = "publish"
			}
			name, err := Name(key, kind)
			if err != nil || len(name) > maxNameBytes || !label.MatchString(name) || seen[name] {
				t.Fatalf("%q: %v", name, err)
			}
			seen[name] = true
		}
	}
}

func TestNameRefusesIncompleteOrInconsistentKeys(t *testing.T) {
	for _, tc := range []struct {
		key  Key
		kind Kind
	}{
		{}, {key: Key{Repository: "repo"}, kind: Pipeline}, {kind: Pipeline},
		{key: Key{Repository: "forge/org/repo"}, kind: "other"},
		{key: Key{Repository: "forge/org/repo"}, kind: Step},
		{key: Key{Repository: "forge/org/repo", File: "file"}, kind: Step},
		{key: Key{Repository: "forge/org/repo", Step: "step"}, kind: Pipeline},
		{key: Key{Repository: "forge/org/repo", File: "file"}, kind: Pipeline},
		{key: Key{Repository: "forge/org/repo\xff"}, kind: Pipeline},
	} {
		name, err := Name(tc.key, tc.kind)
		var invalid *InvalidKeyError
		if name != "" || !errors.As(errors.Join(errors.New("another"), err), &invalid) {
			t.Fatalf("%+v: %q %v", tc, name, err)
		}
		for text, limit := range map[string]int{invalid.Reason(): 80, invalid.Remedy(): 120} {
			if problem := publicerr.CheckMessage(text, limit); problem != nil {
				t.Fatal(problem, text)
			}
		}
	}
	if Kind("").Valid() || Kind("other").Valid() {
		t.Fatal("invalid kind accepted")
	}
}
