package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// A resumed install observes the complete rendered release before deciding
// whether Helm needs another revision. Values alone miss local chart edits.
func resumedOberthUnchanged(ctx context.Context, cfg Config, deps Deps, release helmRelease, openbao OpenBaoResult, rekor RekorResult) (bool, error) {
	if cfg.ResumeSecretsFile == "" || cfg.Upgrade || release.Status != "deployed" {
		return false, nil
	}
	installed := canonicalChartVersion(chartVersionFromRelease(release.Chart, "oberth"))
	if cfg.ChartPath == "" && (installed == "" || installed != canonicalChartVersion(cfg.ChartVersion)) {
		return false, nil
	}
	args := append(OberthHelmArgs(cfg, openbao, rekor), "--dry-run=server", "--no-hooks", "--output=json")
	raw, err := deps.RunHelm(ctx, args)
	if err != nil {
		return false, errors.New("cannot preview resumed Oberth release")
	}
	defer clear(raw)
	var preview struct {
		Config   *map[string]any `json:"config"`
		Manifest string          `json:"manifest"`
		Chart    struct {
			Values map[string]any `json:"values"`
		} `json:"chart"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&preview) != nil || preview.Config == nil || strings.TrimSpace(preview.Manifest) == "" {
		return false, errors.New("resume preview is missing its configuration or manifest")
	}
	planned := preview.Chart.Values
	if planned == nil {
		planned = map[string]any{}
	}
	mergePlannedValues(planned, *preview.Config)
	live, err := readHelmValuesMap(ctx, deps, cfg.Namespace)
	if err != nil {
		return false, err
	}
	var changed []string
	changedValuePaths(live, planned, "", &changed)
	if len(changed) > 0 {
		return false, nil
	}
	manifest, err := deps.RunHelm(ctx, []string{"get", "manifest", "oberth", "-n", cfg.Namespace})
	if err != nil {
		return false, errors.New("cannot read installed Oberth manifest for resume")
	}
	defer clear(manifest)
	return strings.TrimSpace(string(manifest)) == strings.TrimSpace(preview.Manifest), nil
}

func installManualSteps(cfg Config, result InstallResult, installErr error) []string {
	if result.OK {
		return nil
	}
	var steps []string
	if result.Upstream != nil && result.Upstream.State == "pending" {
		steps = append(steps, upstreamKeyRemedy(*result.Upstream))
		if result.Upstream.NextCommand != "" {
			steps = append(steps, result.Upstream.NextCommand)
		}
	} else if cfg.ForgeURL == "" {
		steps = append(steps, "oberth upstream add --yes <upstream> ssh://git@<host>/<organization>")
	}
	target := []string{"install", "--context", cfg.ContextName, "--namespace", cfg.Namespace}
	if installErr != nil && !errors.Is(installErr, ErrOnboardingPartial) {
		resume := append([]string{}, target...)
		saved := cfg.ResumeSecretsFile
		if cfg.SecretsFile != "" {
			saved = cfg.SecretsFile
		}
		if saved != "" {
			resume = append(resume, "--resume-secrets-file", saved)
		}
		if cfg.ForgeURL != "" {
			resume = append(resume, "--upstream-url", cfg.ForgeURL)
		}
		resume = append(resume, "--secrets-file", "<new-secrets-file>", "--confirmed", "--json")
		steps = append(steps, printedAdminCommand(resume...))
	}
	steps = append(steps, printedAdminCommand(append(target, "--verify", "--json")...))
	return steps
}
