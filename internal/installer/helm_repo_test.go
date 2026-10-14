package installer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func TestChartConsumersRefreshHelmRepo(t *testing.T) {
	t.Parallel()
	const version = "0.17.16"
	for _, tc := range []struct {
		name, repo, url, consumer string
		bestEffort                bool
		run                       func(context.Context, Deps) error
	}{
		{"install", oberthRepoName, OberthHelmRepoURL, "upgrade", false, func(ctx context.Context, deps Deps) error {
			_, err := InstallOberth(ctx, Config{ChartVersion: version}, deps, OpenBaoResult{}, RekorResult{})
			return err
		}},
		{"upgrade", oberthRepoName, OberthHelmRepoURL, "upgrade", false, func(ctx context.Context, deps Deps) error {
			_, err := RunUpgrade(ctx, UpgradeConfig{BinaryVersion: version}, deps)
			return err
		}},
		{"openbao", openBaoRepoName, OpenBaoHelmRepoURL, "upgrade", false, func(ctx context.Context, deps Deps) error {
			_, err := InstallOpenBao(ctx, Config{OpenBaoChartVersion: version}, deps)
			return err
		}},
		{"argo", argoRepoName, ArgoHelmRepoURL, "upgrade", false, func(ctx context.Context, deps Deps) error {
			_, err := InstallArgoWorkflows(ctx, Config{ArgoChartVersion: version}, deps)
			return err
		}},
		{"kind images", oberthRepoName, OberthHelmRepoURL, "show", false, func(ctx context.Context, deps Deps) error {
			return prepareKindImagesForCluster(ctx, Config{ChartVersion: version}, deps, "oberth")
		}},
		{"TLS bootstrap", oberthRepoName, OberthHelmRepoURL, "show", false, func(ctx context.Context, deps Deps) error {
			_, err := resolveOberthTLSBootstrapImage(ctx, Config{ChartVersion: version}, deps)
			return err
		}},
		{"newer chart hint", oberthRepoName, OberthHelmRepoURL, "search", true, func(ctx context.Context, deps Deps) error {
			maybeHintNewerOberthChart(ctx, deps, OberthResult{TargetVersion: version})
			return nil
		}},
	} {
		for _, failAt := range []string{"", "add", "update"} {
			t.Run(tc.name+"/failure="+failAt, func(t *testing.T) {
				t.Parallel()
				stop := errors.New("fake Helm failure")
				var repoCalls [][]string
				consumed := false
				deps := Deps{
					Output: io.Discard, KubeClient: fake.NewClientset(),
					RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"},
					RunHelm: func(_ context.Context, args []string) ([]byte, error) {
						switch args[0] {
						case "list":
							return []byte(`[{"name":"oberth","namespace":"oberth","status":"deployed","chart":"oberth-0.17.15"}]`), nil
						case "repo":
							repoCalls = append(repoCalls, slices.Clone(args))
							if args[1] == failAt {
								return nil, stop
							}
							return nil, nil
						}
						if failAt != "" || len(repoCalls) != 2 || !slices.Equal(repoCalls[1], []string{"repo", "update", tc.repo}) {
							t.Fatalf("chart consumed before successful named refresh: %v; calls=%v", args, repoCalls)
						}
						if args[0] == tc.consumer {
							consumed = true
							if tc.consumer == "upgrade" && !strings.Contains(strings.Join(args, " "), "--version "+version) {
								t.Fatalf("upgrade lost target version: %v", args)
							}
							// Stop before rollout or any other host/cluster action.
							return nil, stop
						}
						if args[0] == "show" {
							return []byte(fakeChartValuesYAML), nil
						}
						t.Fatalf("unexpected Helm call: %v", args)
						return nil, stop
					},
				}
				err := tc.run(context.Background(), deps)
				if (!tc.bestEffort && !errors.Is(err, stop)) || (tc.bestEffort && err != nil) {
					t.Fatalf("error = %v; best effort = %v", err, tc.bestEffort)
				}
				wantCalls := [][]string{{"repo", "add", "--force-update", tc.repo, tc.url}}
				if failAt != "add" {
					wantCalls = append(wantCalls, []string{"repo", "update", tc.repo})
				}
				if !slices.EqualFunc(repoCalls, wantCalls, slices.Equal[[]string]) || consumed != (failAt == "") {
					t.Fatalf("repo calls=%v, want=%v; consumed=%v", repoCalls, wantCalls, consumed)
				}
			})
		}
	}
}

func TestDryRunPlansRefreshRegisteredRepos(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"plain", "kind", "production OpenBao", "dev OpenBao", "upgrade"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			cfg := Config{ChartVersion: "0.17.16", InstallSecretStore: kind == "production OpenBao", InstallSecretStoreDev: kind == "dev OpenBao"}
			var err error
			if kind == "upgrade" {
				_, err = printUpgradeDryRun(&out, UpgradeConfig{BinaryVersion: "0.17.16"}, UpgradeResult{TargetVersion: "0.17.16"}, false)
			} else {
				err = printDryRunPlan(cfg, &out, ClusterInfo{Engine: kind})
			}
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(out.String(), "\n")
			adds := 0
			for i, line := range lines {
				fields := strings.Fields(line)
				if len(fields) != 6 || strings.Join(fields[:4], " ") != "helm repo add --force-update" {
					continue
				}
				adds++
				if i+1 >= len(lines) || strings.TrimSpace(lines[i+1]) != "helm repo update "+fields[4] {
					t.Fatalf("printed repo registration lacks immediate refresh: %s", out.String())
				}
			}
			if adds == 0 {
				t.Fatal("plan contains no repo registration")
			}
		})
	}
}
