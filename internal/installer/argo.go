package installer

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const (
	// DefaultArgoNamespace is the namespace pipelines execute in. It is
	// deliberately not Oberth's own: the OpenBao Kubernetes-auth roles bind
	// (namespace, ServiceAccount) pairs, so a pipeline identity must not sit
	// beside a server one. The Oberth chart refuses to render if the two match.
	DefaultArgoNamespace = "oberth-argo"

	// DefaultArgoChartVersion is the pinned argo-workflows chart version
	// (appVersion v4.0.8 — the controller build the execution engine was
	// proven against end to end). Bump deliberately and re-run the live engine
	// suite; never float to latest.
	DefaultArgoChartVersion = "1.0.24"

	// ArgoHelmRepoURL is the canonical Argo Helm chart repository.
	ArgoHelmRepoURL = "https://argoproj.github.io/argo-helm"

	argoRepoName = "argo"

	// argoControllerLabelSelector matches the workflow-controller Pod the
	// chart deploys.
	argoControllerLabelSelector = "app.kubernetes.io/name=argo-workflows-workflow-controller"

	// podNamesEnv pins the controller's pod naming scheme. Oberth stamps
	// workflows.argoproj.io/pod-name-format: v2 on every submission so it can
	// compute a step Pod's name to stream its log; if the controller disagrees,
	// nothing errors — the logs simply never appear. Pinning both ends removes
	// the disagreement rather than relying on a shared default.
	podNamesEnv    = "POD_NAMES"
	podNamesFormat = "v2"
)

// ArgoResult describes the outcome of the Argo Workflows install phase.
type ArgoResult struct {
	AlreadyInstalled bool
	// Upgraded reports that an existing release was moved to TargetVersion
	// (or force-reconciled with --upgrade) instead of freshly installed.
	Upgraded bool
	// InstalledVersion is the chart version found before any action ("" when
	// the release was absent or its version could not be determined).
	InstalledVersion string
	// TargetVersion is the chart version the install aimed at.
	TargetVersion string
	Namespace     string
	// Skipped reports that the caller opted out with --skip-argo-workflows,
	// having supplied a controller by other means.
	Skipped bool
}

// InstallArgoWorkflows installs the workflow controller that executes every
// Oberth pipeline.
//
// This is not an optional add-on the way the Rekor witness is: Argo is the
// execution engine, so a cluster without a controller watching the pipeline
// namespace can accept pushes and then run nothing. It is installed by default
// and opted out of only when a controller is supplied by other means.
//
// The install is deliberately the smallest one the chart can express:
//
//   - singleNamespace, so the controller runs with --namespaced and its
//     permissions are a Role in one namespace rather than a ClusterRole.
//   - no Argo Server. Oberth submits through the Kubernetes API with its own
//     ServiceAccount; the server is a second authenticated HTTP surface, with
//     its own authentication modes and its own exposure, that nothing here
//     would call.
//   - no aggregate roles and no ClusterWorkflowTemplate access. Oberth's
//     admission refuses workflowTemplateRef and refuses to decode anything but
//     a concrete Workflow, so standing cluster-scoped permission to read
//     objects it will never reference is privilege with no counterpart.
//   - no chart-created workflow ServiceAccount or RBAC. The three identities
//     the trigger gate rests on ship with the Oberth chart, which is what
//     keeps them versioned with the code that forces them.
//
// The result is an install with zero cluster-scoped objects.
func InstallArgoWorkflows(ctx context.Context, cfg Config, deps Deps) (ArgoResult, error) {
	ns := cfg.ArgoNamespace
	if ns == "" {
		ns = DefaultArgoNamespace
	}
	result := ArgoResult{Namespace: ns}
	if cfg.SkipArgo {
		result.Skipped = true
		return result, nil
	}
	oberthNS := cfg.Namespace
	if oberthNS == "" {
		oberthNS = DefaultNamespace
	}
	if ns == oberthNS {
		return result, fmt.Errorf("--argo-namespace %q must differ from Oberth's namespace: "+
			"OpenBao Kubernetes-auth roles bind (namespace, ServiceAccount) pairs, "+
			"so pipeline identities must not sit beside server ones", ns)
	}

	if _, err := deps.RunHelm(ctx, []string{"repo", "add", "--force-update", argoRepoName, ArgoHelmRepoURL}); err != nil {
		return result, fmt.Errorf("add Argo helm repo: %w", err)
	}

	release, exists := findHelmRelease(ctx, deps, "argo-workflows", ns)
	result.InstalledVersion = chartVersionFromRelease(release.Chart, "argo-workflows")
	result.TargetVersion = cfg.ArgoChartVersion
	if result.TargetVersion == "" {
		result.TargetVersion = DefaultArgoChartVersion
	}

	switch planHelmAction(exists, result.InstalledVersion, result.TargetVersion, release.Status, cfg.Upgrade) {
	case actionSkip:
		result.AlreadyInstalled = true
		return result, nil
	case actionUpgrade:
		result.Upgraded = true
		if result.InstalledVersion != "" && result.InstalledVersion != result.TargetVersion {
			_, _ = fmt.Fprintf(deps.Output, "Argo Workflows chart %s installed, %s available. Upgrading...\n",
				displayVersion(result.InstalledVersion), displayVersion(result.TargetVersion))
		} else {
			_, _ = fmt.Fprintln(deps.Output, "Upgrading Argo Workflows...")
		}
	case actionInstall:
		_, _ = fmt.Fprintln(deps.Output, "Installing Argo Workflows...")
	}

	if _, err := deps.RunHelm(ctx, ArgoHelmArgs(cfg)); err != nil {
		return result, fmt.Errorf("helm install Argo Workflows: %w", err)
	}
	return result, nil
}

// ArgoHelmArgs returns the exact helm arguments for installing the workflow
// controller. Every --set here is load-bearing; see InstallArgoWorkflows for
// why each one is set the way it is.
func ArgoHelmArgs(cfg Config) []string {
	ns := cfg.ArgoNamespace
	if ns == "" {
		ns = DefaultArgoNamespace
	}
	chartVersion := cfg.ArgoChartVersion
	if chartVersion == "" {
		chartVersion = DefaultArgoChartVersion
	}
	return []string{
		"upgrade", "--install", "argo-workflows", argoRepoName + "/argo-workflows",
		"-n", ns, "--create-namespace",
		"--set", "singleNamespace=true",
		"--set", "server.enabled=false",
		"--set", "createAggregateRoles=false",
		"--set", "controller.clusterWorkflowTemplates.enabled=false",
		"--set", "workflow.serviceAccount.create=false",
		"--set", "workflow.rbac.create=false",
		// Minified CRDs: the full schemas install through a pre-install hook
		// Job that server-side-applies them, which is a privileged moving part
		// during install. Nothing is lost by skipping it — Oberth decodes every
		// document strictly into the typed Workflow before submission, so an
		// unknown field is already a rejected push rather than a field the API
		// server would have had to catch.
		"--set", "crds.install=true",
		"--set", "crds.keep=true",
		"--set", "crds.full=false",
		"--set", "controller.extraEnv[0].name=" + podNamesEnv,
		"--set-string", "controller.extraEnv[0].value=" + podNamesFormat,
		"--version", chartVersion,
		// Idempotent re-runs must not reset values set on a previous install;
		// the --set flags above still override reused values.
		"--reuse-values",
		"--wait",
	}
}

// VerifyArgoController checks that a controller is actually watching the
// pipeline namespace, whether this installer put it there or not.
//
// --skip-argo-workflows is a claim that one exists. Taking the claim on faith
// means the first push after install queues a Workflow that nothing reconciles,
// and the run simply never starts — a failure with no error anywhere. This
// turns that into a message at install time.
func VerifyArgoController(ctx context.Context, cfg Config, deps Deps) error {
	ns := cfg.ArgoNamespace
	if ns == "" {
		ns = DefaultArgoNamespace
	}
	if deps.KubeClient == nil {
		return errors.New("verify Argo controller: no Kubernetes client")
	}
	if err := waitForPodReady(ctx, deps, ns, argoControllerLabelSelector, cfg.Timeout); err != nil {
		return fmt.Errorf("no ready Argo workflow controller found in namespace %s: %w\n"+
			"Oberth submits every pipeline as an Argo Workflow; without a controller watching %s "+
			"a push is accepted and then never executes", ns, err, ns)
	}
	return nil
}

// argoOberthHelmValues returns the Oberth chart values that bind the server to
// the controller this phase installed.
//
// The namespace is always set: skipping the controller install means the
// operator supplied one, not that pipelines execute some other way. There is no
// enable flag, because there is no second execution path to switch to.
//
// When the installer manages a secret store and no explicit --argo-vault-address
// was given, the effective address defaults to the installed store's service
// address. This ensures that the first push after a default
// `oberth install --install-secretstore` produces credentialed release steps
// that can reach the installed store — without this, argo.vault.address stays
// empty and envconsul dials its built-in 127.0.0.1:8200 default, failing every
// release at tag time.
func argoOberthHelmValues(cfg Config, openbao OpenBaoResult) []string {
	ns := cfg.ArgoNamespace
	if ns == "" {
		ns = DefaultArgoNamespace
	}
	values := []string{"--set", "argo.namespace=" + ns}
	address := strings.TrimSpace(cfg.ArgoVaultAddress)
	// When the installer manages a production secret store and the caller did
	// not explicitly point pipelines at a different vault, default to the
	// store the installer just provisioned. The HTTPS guard mirrors the
	// validator's own rejection of plaintext vault addresses: release-tier
	// credentials traverse the network, and dev-mode HTTP stores are not
	// wired here because they cannot serve credentialed releases. The
	// CA-pinning condition below then fires automatically because address
	// equals ServiceAddress.
	if address == "" && cfg.wantsSecretStore() {
		if svc := strings.TrimSpace(openbao.ServiceAddress); svc != "" && strings.HasPrefix(svc, "https://") {
			address = svc
		}
	}
	if address != "" {
		values = append(values, "--set", "argo.vault.address="+address)
	}
	if role := strings.TrimSpace(cfg.ArgoVaultCredentialedRole); role != "" {
		values = append(values, "--set", "argo.vault.credentialedRole="+role)
	}
	// The credentialed and ci-secrets roles are always set when the installer
	// manages the store, because ConfigureSecretStore creates both. The chart
	// values enable the server to inject the trigger's own role into its
	// containers as OBERTH_VAULT_ROLE. Pinned explicitly here rather than
	// trusted to chart defaults: `helm upgrade --reuse-values` never consults
	// a new chart's values.yaml, so an unpinned new value stays absent on
	// every upgraded install.
	if cfg.wantsSecretStore() {
		values = append(values,
			"--set", "argo.vault.credentialedRole="+defaultCredentialedRole,
			"--set", "argo.ciSecrets.vaultRole="+defaultCISecretsRole,
		)
	}
	// The installer issues the OpenBao serving certificate from a CA of its
	// own, so a release-tier pipeline has no way to verify that endpoint unless
	// the installer hands the anchor on. It does so only for the exact endpoint
	// it installed: pointing pipelines at a different store and pinning this CA
	// anyway would break every release with a verification failure that names
	// the wrong cause.
	if address != "" && address == strings.TrimSpace(openbao.ServiceAddress) &&
		strings.TrimSpace(openbao.CACertPEM) != "" {
		values = append(values, "--set-string", "argo.vault.caCert="+openbao.CACertPEM)
	}
	return values
}
