package installer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// This opt-in transition touches only the already managed Argo release and the
// normal Oberth upgrade. It never reconciles Secretstore, namespaces or uplinks.
type upgradeControllerProfile struct {
	namespace    string
	status       string
	revision     string
	valuesFile   string
	valuesDigest [32]byte
	chartPath    string
	chartDigest  string
}

func checkControllerProfileOverrides(cfg UpgradeConfig) error {
	refused := func(key string) bool {
		for _, fixed := range []string{"argo.namespace", "argo.controllerProfile"} {
			if key == fixed || strings.HasPrefix(fixed, key+".") || strings.HasPrefix(key, fixed+".") {
				return true
			}
		}
		return false
	}
	for _, kv := range cfg.UserSet {
		key, err := upgradeSetKey(kv)
		if err != nil {
			return err
		}
		if refused(key) {
			return errors.New("controller profile transition refuses namespace/profile overrides")
		}
	}
	for _, path := range cfg.UserValues {
		raw, err := os.ReadFile(path) //nolint:gosec // Existing explicit --values input, never serialized.
		if err != nil {
			return errors.New("cannot inspect controller profile overrides")
		}
		var values map[string]any
		if yaml.Unmarshal(raw, &values) != nil {
			return errors.New("cannot decode controller profile overrides")
		}
		paths := map[string]bool{}
		collectLeafKeyPaths(values, "", paths)
		for key := range paths {
			if refused(key) {
				return errors.New("controller profile transition refuses namespace/profile overrides")
			}
		}
	}
	return nil
}

func installedPipelineNamespace(ctx context.Context, cfg UpgradeConfig, deps Deps) (string, error) {
	raw, err := deps.RunHelm(ctx, []string{"get", "values", "oberth", "-n", cfg.Namespace, "--all", "-o", "json"})
	if err != nil {
		return "", errors.New("cannot inspect installed pipeline namespace")
	}
	var values struct {
		Argo struct {
			Namespace string `json:"namespace"`
		} `json:"argo"`
	}
	if json.Unmarshal(raw, &values) != nil || len(validation.IsDNS1123Label(values.Argo.Namespace)) != 0 || values.Argo.Namespace == cfg.Namespace {
		return "", errors.New("installed pipeline namespace refused")
	}
	return values.Argo.Namespace, nil
}

func prepareUpgradeControllerProfile(ctx context.Context, cfg UpgradeConfig, deps Deps) (*upgradeControllerProfile, func(), error) {
	noop := func() {}
	if cfg.ArgoControllerProfile == "" {
		return nil, noop, nil
	}
	if cfg.ArgoControllerProfile != argoworkflow.NonrootStaticProfile {
		return nil, noop, errors.New("unsupported Argo controller profile")
	}
	if err := checkControllerProfileOverrides(cfg); err != nil {
		return nil, noop, err
	}
	namespace, err := installedPipelineNamespace(ctx, cfg, deps)
	if err != nil {
		return nil, noop, err
	}
	release, exists := findHelmRelease(ctx, deps, "argo-workflows", namespace)
	// Failed exact releases may be forward repaired. Pending/unknown/absent
	// releases never authorize installing, adopting or downgrading a controller.
	if !exists || release.Chart != "argo-workflows-"+DefaultArgoChartVersion || (release.Status != "deployed" && release.Status != "failed") || helmRevisionLabel(release.Revision) == "" {
		return nil, noop, errors.New("controller transition requires existing managed Argo chart1.0.24 in deployed or failed state")
	}
	file, cleanup, err := prepareNonrootArgoValues(ctx, Config{Namespace: cfg.Namespace, ArgoNamespace: namespace, ArgoControllerProfile: cfg.ArgoControllerProfile}, deps, true, DefaultArgoChartVersion)
	if err != nil {
		return nil, noop, err
	}
	raw, err := deps.RunHelm(ctx, []string{"get", "values", "argo-workflows", "-n", namespace, "-o", "json"})
	if err != nil {
		cleanup()
		return nil, noop, errors.New("cannot bind existing Argo values")
	}
	var values map[string]any
	if json.Unmarshal(raw, &values) != nil || validateNonrootReuse(values, namespace) != nil {
		cleanup()
		return nil, noop, errors.New("changed or unsupported Argo values")
	}
	return &upgradeControllerProfile{namespace: namespace, status: release.Status, revision: helmRevisionLabel(release.Revision), valuesFile: file, valuesDigest: sha256.Sum256(raw), chartDigest: "7b1d540095ba32bb8c914432b554c64809b47a6f737b1448273d3a4740be3d50"}, cleanup, nil
}

func (p *upgradeControllerProfile) args(timeout time.Duration) []string {
	return []string{"upgrade", "argo-workflows", p.chartPath, "-n", p.namespace, "--version", DefaultArgoChartVersion, "--reuse-values", "--values", p.valuesFile, "--timeout", timeout.String(), "--wait"}
}

func (p *upgradeControllerProfile) describe(w io.Writer, timeout time.Duration) {
	preview := *p
	preview.chartPath = "<sha256:" + p.chartDigest + ">"
	_, _ = fmt.Fprintf(w, "  helm %s\n", strings.Join(preview.args(timeout), " "))
	overlay, _ := json.Marshal(nonrootArgoValues(p.namespace))
	_, _ = fmt.Fprintf(w, "  Built-in public overlay: %s\n", overlay)
	_, _ = fmt.Fprintf(w, "Controller transition: existing Argo chart%s in namespace %s; preserve reused values, apply only built-in %s (config SHA256 %s; controller %s; executor %s), wait and verify exact live profile, then enable Oberth admission. No automatic rollback.\n", DefaultArgoChartVersion, p.namespace, argoworkflow.NonrootStaticProfile, argoworkflow.NonrootProfileConfigSHA256, argoworkflow.NonrootControllerImage, argoworkflow.NonrootExecutorImage)
}

func (p *upgradeControllerProfile) verifySnapshot(ctx context.Context, deps Deps) error {
	release, exists := findHelmRelease(ctx, deps, "argo-workflows", p.namespace)
	raw, err := deps.RunHelm(ctx, []string{"get", "values", "argo-workflows", "-n", p.namespace, "-o", "json"})
	if err != nil || !exists || release.Chart != "argo-workflows-"+DefaultArgoChartVersion || release.Status != p.status || helmRevisionLabel(release.Revision) != p.revision || sha256.Sum256(raw) != p.valuesDigest {
		return errors.New("argo release or values changed after transition preparation")
	}
	return nil
}

func (p *upgradeControllerProfile) apply(ctx context.Context, cfg UpgradeConfig, deps Deps) error {
	if deps.KubeClient == nil {
		return errors.New("controller transition requires live Kubernetes verification")
	}
	if err := p.verifySnapshot(ctx, deps); err != nil {
		return err
	}
	if err := refreshHelmRepo(ctx, deps, argoRepoName, ArgoHelmRepoURL); err != nil {
		return errors.New("cannot resolve pinned Argo chart")
	}
	chartDir, err := os.MkdirTemp("", "oberth-controller-chart-*")
	if err != nil {
		return errors.New("cannot allocate private controller chart directory")
	}
	defer func() { _ = os.RemoveAll(chartDir) }()
	if _, err := deps.RunHelm(ctx, []string{"pull", argoRepoName + "/argo-workflows", "--version", DefaultArgoChartVersion, "--destination", chartDir}); err != nil {
		return errors.New("cannot fetch pinned controller chart")
	}
	p.chartPath = filepath.Join(chartDir, "argo-workflows-"+DefaultArgoChartVersion+".tgz")
	if err := verifyControllerChart(p.chartPath, p.chartDigest); err != nil {
		return err
	}
	// Network resolution must not leave a stale --reuse-values snapshot.
	// Helm owns its operation lock; this is not a cross-release CAS.
	if err := p.verifySnapshot(ctx, deps); err != nil {
		return err
	}
	if _, err := deps.RunHelm(ctx, p.args(cfg.Timeout)); err != nil {
		return errors.New("argo profile upgrade failed; preserve profile and retry supported forward repair")
	}
	deadline, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	for {
		if argojob.VerifyNonrootControllerProfile(deadline, deps.KubeClient, p.namespace) == nil {
			return nil
		}
		select {
		case <-deadline.Done():
			return errors.New("argo profile readback failed; Oberth upgrade not attempted")
		case <-time.After(2 * time.Second):
		}
	}
}

func (p *upgradeControllerProfile) verifyFinal(ctx context.Context, cfg UpgradeConfig, deps Deps) error {
	if err := argojob.VerifyNonrootControllerProfile(ctx, deps.KubeClient, p.namespace); err != nil {
		return errors.New("controller profile changed after Oberth upgrade")
	}
	namespace, err := installedPipelineNamespace(ctx, cfg, deps)
	if err != nil || namespace != p.namespace {
		return errors.New("oberth pipeline namespace readback refused")
	}
	raw, readErr := deps.RunHelm(ctx, []string{"get", "values", "oberth", "-n", cfg.Namespace, "--all", "-o", "json"})
	var values struct {
		Argo struct {
			Profile string `json:"controllerProfile"`
		} `json:"argo"`
	}
	if readErr != nil || json.Unmarshal(raw, &values) != nil || values.Argo.Profile != argoworkflow.NonrootStaticProfile {
		return errors.New("oberth controller profile values readback refused")
	}
	deployments, err := deps.KubeClient.AppsV1().Deployments(cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/instance=oberth"})
	if err != nil || len(deployments.Items) != 1 {
		return errors.New("oberth controller-profile deployment readback failed")
	}
	deployment := &deployments.Items[0]
	found := false
	for _, c := range deployment.Spec.Template.Spec.Containers {
		if c.Name != "oberth" {
			continue
		}
		count := 0
		for _, arg := range c.Args {
			if strings.HasPrefix(arg, "--argo-controller-profile") {
				if arg != "--argo-controller-profile="+argoworkflow.NonrootStaticProfile {
					return errors.New("oberth controller profile readback refused")
				}
				count++
			}
		}
		found = count == 1
	}
	if !found {
		return errors.New("oberth controller profile flag missing or duplicated")
	}
	return verifyControllerServer(ctx, cfg, deps, deployment)
}

func verifyControllerChart(path, digest string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 16<<20 {
		return errors.New("controller chart file refused")
	}
	file, err := os.Open(path) //nolint:gosec // Fixed file in a newly owned private directory.
	if err != nil {
		return errors.New("controller chart read refused")
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, (16<<20)+1))
	sum := sha256.Sum256(raw)
	if err != nil || len(raw) > 16<<20 || hex.EncodeToString(sum[:]) != digest {
		return errors.New("controller chart digest refused")
	}
	return nil
}
