package installer

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestManagedArgoExecutorPinFollowsEffectiveChartVersion(t *testing.T) {
	cap := 6
	for _, test := range []struct {
		name, installed, target     string
		parallelism                 *int
		upgrade, write, pin, mirror bool
	}{
		{name: "installed nondefault", installed: "1.0.30"},
		{name: "installed nondefault mirror", installed: "1.0.30", mirror: true},
		{name: "installed nondefault cap", installed: "1.0.30", parallelism: &cap, write: true},
		{name: "fresh explicit nondefault", target: "1.0.30", write: true},
		{name: "upgrade to nondefault", installed: DefaultArgoChartVersion, target: "1.0.30", write: true},
		{name: "fresh default", write: true, pin: true},
		{name: "fresh explicit default", target: "v" + DefaultArgoChartVersion, write: true, pin: true},
		{name: "installed default missing pin", installed: DefaultArgoChartVersion, write: true, pin: true},
		{name: "explicit return to default", installed: "1.0.30", target: DefaultArgoChartVersion, upgrade: true, write: true, pin: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output strings.Builder
			writes := 0
			var applied []string
			// A custom stored image demonstrates that nondefault charts skip the pin's
			// compatibility check as well as its Helm flag.
			stored := `{}`
			if test.mirror {
				stored = `{"executor":{"image":{"registry":"mirror.invalid","tag":"operator"}}}`
			}
			deps := Deps{Output: &output, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
				switch args[0] {
				case "list":
					if test.installed == "" {
						return []byte(`[]`), nil
					}
					return []byte(fmt.Sprintf(`[{"name":"argo-workflows","chart":"argo-workflows-%s","status":"deployed"}]`, test.installed)), nil
				case "get":
					return []byte(stored), nil
				case "upgrade":
					writes++
					applied = slices.Clone(args)
				}
				return nil, nil
			}}
			cfg := Config{ArgoChartVersion: test.target, ArgoParallelism: test.parallelism, Upgrade: test.upgrade}
			result, err := InstallArgoWorkflows(t.Context(), cfg, deps)
			if err != nil || (writes == 1) != test.write || writes > 1 {
				t.Fatal(result, writes, err)
			}
			if !test.write && (!result.AlreadyInstalled || result.Upgraded) {
				t.Fatal("nondefault rerun changed release", result)
			}
			pin := false
			for _, arg := range applied {
				pin = pin || strings.HasPrefix(arg, "executor.image.")
			}
			if pin != (test.pin && test.write) {
				t.Fatal("unexpected executor override", applied)
			}
			notices := strings.Count(output.String(), "executor image left unchanged; resource-template token delivery requires ")
			wantNotices := 1
			if test.pin {
				wantNotices = 0
			}
			if notices != wantNotices || (!test.pin && !strings.Contains(output.String(), argoworkflow.ExecutorImage)) {
				t.Fatal(output.String())
			}
			if test.write && !test.pin {
				index := slices.Index(applied, "--version")
				if index < 0 || strings.TrimPrefix(applied[index+1], "v") != "1.0.30" {
					t.Fatal("wrong effective chart", applied)
				}
			}
			if test.target != "" && !test.pin {
				for _, arg := range ArgoHelmArgs(cfg) {
					if strings.HasPrefix(arg, "executor.image.") {
						t.Fatal("explicit nondefault chart pinned", arg)
					}
				}
			}
		})
	}
}

func TestManagedArgoExternalConfigMapParallelismNotice(t *testing.T) {
	cap := 6
	for _, test := range []struct {
		name, values string
		parallelism  *int
		want         int
	}{
		{"external requested", `{"controller":{"configMap":{"create":false}}}`, &cap, 1},
		{"native requested", `{"controller":{"configMap":{"create":true}}}`, &cap, 0},
		{"default requested", `{}`, &cap, 0},
		{"external no request", `{"controller":{"configMap":{"create":false}}}`, nil, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output strings.Builder
			deps := Deps{Output: &output, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
				switch args[0] {
				case "list":
					return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"deployed"}]`), nil
				case "get":
					return []byte(test.values), nil
				}
				return nil, nil
			}}
			if _, err := InstallArgoWorkflows(t.Context(), Config{ArgoParallelism: test.parallelism}, deps); err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(output.String(), "controller.configMap.create=false: --argo-parallelism"); got != test.want {
				t.Fatal(got, output.String())
			}
			if test.want > 0 && !strings.Contains(output.String(), "docs/releases/v0.22.4.md") {
				t.Fatal("migration remedy absent", output.String())
			}
		})
	}
}
