package argoworkflow

import (
	core "k8s.io/api/core/v1"
	"strings"
	"testing"
)

func TestMountSubPaths(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"container", "script", "init", "sidecar", "member", "set", "defaults", "unused", "inline", "reference"} {
		t.Run(kind, func(t *testing.T) {
			for _, tc := range []struct {
				value   string
				refused bool
			}{
				{"", false}, {"data", false}, {"dir/file", false}, {"dir//file", false}, {"./data", false}, {"..data", false},
				{"..", true}, {"../data", true}, {"data/..", true}, {"dir/../data", true}, {"dir//../data", true}, {"/data", true},
			} {
				sub := tc.value
				t.Run(sub, func(t *testing.T) {
					w, p := mountSpecDocument(kind, core.VolumeMount{Name: "data", MountPath: "/safe", SubPath: sub})
					_, err := Prepare(t.Context(), requestFor(t, w), p, identityHooks())
					if !tc.refused {
						if err != nil {
							t.Fatal("positive control", err)
						}
						return
					}
					assertMountRefusal(t, err, sub)
					if !strings.Contains(err.Error(), "subPath") {
						t.Fatal("subPath missing", err)
					}
				})
			}
		})
	}
}

func TestMountSubPathExpressionsUnchanged(t *testing.T) {
	t.Parallel()
	for _, expr := range []string{"$(DIRECTORY)", "../data", "/data"} {
		w, p := mountDocument("container", "/safe")
		w.Spec.Templates[0].Container.VolumeMounts[0].SubPathExpr = expr
		if _, err := Prepare(t.Context(), requestFor(t, w), p, identityHooks()); err != nil {
			t.Fatal("ordinary expression refused", err)
		}
		w, f := tokenWorkflow()
		w.Spec.Templates[0].Container.VolumeMounts[0].SubPathExpr = expr
		requireCode(t, admitFixture(t.Context(), w, f), "token")
	}
}
