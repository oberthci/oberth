package argojob_test

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/sourcecontract"
	core "k8s.io/api/core/v1"
	"slices"
	"testing"
)

func TestSourceTrustCountsOnlyTheExactAuthoredEntry(t *testing.T) {
	t.Parallel()
	entry := func(keys, values []string) []core.EnvVar {
		env := []core.EnvVar{{Name: "GIT_CONFIG_COUNT", Value: string(rune('0' + len(keys)))}}
		for i := range keys {
			n := string(rune('0' + i))
			env = append(env, core.EnvVar{Name: "GIT_CONFIG_KEY_" + n, Value: keys[i]}, core.EnvVar{Name: "GIT_CONFIG_VALUE_" + n, Value: values[i]})
		}
		return env
	}
	sd := "safe.directory"
	for _, tc := range []struct {
		name   string
		env    []core.EnvVar
		trusts bool
	}{
		{"exact", entry([]string{sd}, []string{"/work/src"}), true},
		{"key case", entry([]string{"Safe.Directory"}, []string{"/work/src"}), true},
		{"trust after reset", entry([]string{sd, sd}, []string{"", "/work/src"}), true},
		// An older Git in the step image compares these spellings literally.
		{"trailing slash", entry([]string{sd}, []string{"/work/src/"}), false},
		{"repeated separators", entry([]string{sd}, []string{"//work//src//"}), false},
		{"dot segments", entry([]string{sd}, []string{"/./work/./src/."}), false},
		{"reset after trust", entry([]string{sd, sd}, []string{"/work/src", ""}), false},
		{"other directory", entry([]string{sd}, []string{"/work/src2"}), false},
		{"relative", entry([]string{sd}, []string{"work/src"}), false},
		{"parent segment", entry([]string{sd}, []string{"/work/x/../src"}), false},
		{"unicode key folding", entry([]string{"ſafe.directory"}, []string{"/work/src"}), false},
		{"other key", entry([]string{"safe.directoryx"}, []string{"/work/src"}), false},
		{"padded index", []core.EnvVar{{Name: "GIT_CONFIG_COUNT", Value: "1"}, {Name: "GIT_CONFIG_KEY_00", Value: sd}, {Name: "GIT_CONFIG_VALUE_00", Value: "/work/src"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			authored := slices.Clone(tc.env)
			w := &wf.Workflow{Spec: wf.WorkflowSpec{Templates: []wf.Template{{Name: "build", Container: &core.Container{Env: slices.Clone(tc.env)}}}}}
			in := admissioncontract.TransformInput{SourceBindings: []sourcecontract.Binding{{Template: "build", Descriptor: sourcecontract.Descriptor{Kind: sourcecontract.Main}}}}
			compositionOK(t, argojob.New(argojob.Config{}).Hooks(nativebinding.Preparation{Repository: "forge/org/repo"}).TransformNative(t.Context(), w, in))
			env := w.Spec.Templates[0].Container.Env
			if tc.trusts {
				if !slices.Equal(env, authored) {
					t.Fatalf("authored entry rewritten or duplicated: %+v", env)
				}
				return
			}
			if len(env) != len(authored)+2 || env[len(env)-1].Value != "/work/src" || !slices.Equal(env[1:len(authored)], authored[1:]) {
				t.Fatalf("trust not appended after authored entries: %+v", env)
			}
		})
	}
}
