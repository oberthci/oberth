package installer

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func argoTestDeps(t *testing.T, recorded *[][]string) Deps {
	t.Helper()
	return Deps{
		Output:     io.Discard,
		KubeClient: fake.NewClientset(),
		RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"},
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			*recorded = append(*recorded, slices.Clone(args))
			if len(args) > 0 && args[0] == "list" {
				return []byte("[]"), nil
			}
			return nil, nil
		},
		PollInterval: time.Millisecond,
	}
}

// The controller's permissions must be a Role in one namespace. A ClusterRole
// would put pipeline execution above the namespace boundary the whole trigger
// gate is expressed in.
func TestArgoHelmArgsInstallsNamespaceScopedControllerOnly(t *testing.T) {
	t.Parallel()
	cfg := Config{}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(ArgoHelmArgs(cfg), " ")

	for _, want := range []string{
		"-n oberth-argo --create-namespace",
		"--set singleNamespace=true",
		"--set createAggregateRoles=false",
		"--set controller.clusterWorkflowTemplates.enabled=false",
		"--version " + DefaultArgoChartVersion,
	} {
		if !strings.Contains(args, want) {
			t.Errorf("argo helm args missing %q:\n%s", want, args)
		}
	}
}

// Oberth submits Workflows through the Kubernetes API with its own
// ServiceAccount. The Argo Server is a second authenticated HTTP surface that
// nothing here calls, so installing it would be exposure with no consumer.
func TestArgoHelmArgsDoesNotInstallTheArgoServer(t *testing.T) {
	t.Parallel()
	cfg := Config{}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(ArgoHelmArgs(cfg), "server.enabled=false") {
		t.Fatalf("argo helm args must disable the Argo Server:\n%s", strings.Join(ArgoHelmArgs(cfg), " "))
	}
}

// The three identities the trigger gate rests on ship with the Oberth chart,
// versioned with the code that forces them. The Argo chart must not create a
// fourth, differently-shaped one.
func TestArgoHelmArgsLeavesWorkflowIdentitiesToTheOberthChart(t *testing.T) {
	t.Parallel()
	cfg := Config{}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	args := ArgoHelmArgs(cfg)
	for _, want := range []string{"workflow.serviceAccount.create=false", "workflow.rbac.create=false"} {
		if !slices.Contains(args, want) {
			t.Errorf("argo helm args must set %q", want)
		}
	}
}

// If the controller computes step Pod names differently from the server, log
// capture fails silently — no error, just missing logs. Both ends are pinned.
func TestArgoHelmArgsPinsThePodNameFormat(t *testing.T) {
	t.Parallel()
	cfg := Config{}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(ArgoHelmArgs(cfg), " ")
	if !strings.Contains(args, "controller.extraEnv[0].name=POD_NAMES") ||
		!strings.Contains(args, "controller.extraEnv[0].value=v2") {
		t.Fatalf("argo helm args must pin POD_NAMES=v2:\n%s", args)
	}
}

func TestInstallArgoWorkflowsInstallsWhenAbsent(t *testing.T) {
	t.Parallel()
	var recorded [][]string
	deps := argoTestDeps(t, &recorded)
	cfg := Config{}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	result, err := InstallArgoWorkflows(context.Background(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	if result.AlreadyInstalled || result.Skipped {
		t.Fatalf("expected a fresh install, got %+v", result)
	}
	if result.Namespace != DefaultArgoNamespace {
		t.Fatalf("namespace = %q, want %q", result.Namespace, DefaultArgoNamespace)
	}
	var upgraded bool
	for _, args := range recorded {
		if len(args) > 3 && args[0] == "upgrade" && args[3] == "argo/argo-workflows" {
			upgraded = true
		}
	}
	if !upgraded {
		t.Fatalf("argo-workflows chart was never installed: %v", recorded)
	}
}

// --skip-argo-workflows is a claim that a controller exists, not a claim that
// pipelines run some other way: the binding to the namespace still happens.
func TestInstallArgoWorkflowsSkipStillBindsOberthToTheNamespace(t *testing.T) {
	t.Parallel()
	var recorded [][]string
	deps := argoTestDeps(t, &recorded)
	cfg := Config{SkipArgo: true}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	result, err := InstallArgoWorkflows(context.Background(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Skipped {
		t.Fatal("expected the install to be skipped")
	}
	if len(recorded) != 0 {
		t.Fatalf("skipping must not run helm: %v", recorded)
	}
	args := argoOberthHelmValues(cfg, OpenBaoResult{})
	if !slices.Contains(args, "argo.namespace="+DefaultArgoNamespace) {
		t.Fatalf("skipping the controller install must still bind the namespace: %v", args)
	}
}

// A controller that is not there means every accepted push queues a Workflow
// nothing reconciles: a run that never starts and never fails.
func TestVerifyArgoControllerFailsWhenNoControllerIsRunning(t *testing.T) {
	t.Parallel()
	deps := Deps{
		Output:       io.Discard,
		KubeClient:   fake.NewClientset(),
		RestConfig:   &rest.Config{Host: "https://127.0.0.1:6443"},
		PollInterval: time.Millisecond,
	}
	cfg := Config{Timeout: 50 * time.Millisecond}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	err := VerifyArgoController(context.Background(), cfg, deps)
	if err == nil {
		t.Fatal("expected an error when no controller is running")
	}
	if !strings.Contains(err.Error(), "never executes") {
		t.Fatalf("error should explain the consequence, got: %v", err)
	}
}

func TestVerifyArgoControllerAcceptsAReadyController(t *testing.T) {
	t.Parallel()
	deps := Deps{
		Output:       io.Discard,
		KubeClient:   fake.NewClientset(readyArgoControllerPod()),
		RestConfig:   &rest.Config{Host: "https://127.0.0.1:6443"},
		PollInterval: time.Millisecond,
	}
	cfg := Config{Timeout: time.Second}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := VerifyArgoController(context.Background(), cfg, deps); err != nil {
		t.Fatal(err)
	}
}

// Vault validates identity, not intent: a pipeline ServiceAccount sitting in
// the server's namespace would be one bound_service_account_namespaces typo
// away from the server's own role.
func TestValidateRejectsArgoNamespaceEqualToOberthNamespace(t *testing.T) {
	t.Parallel()
	cfg := Config{Namespace: "ci", ArgoNamespace: "ci"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected an error when the two namespaces match")
	}
	if !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// VAULT_ADDR is injected into the containers that exchange a real credential.
func TestValidateRejectsPlaintextArgoVaultAddress(t *testing.T) {
	t.Parallel()
	cfg := Config{ArgoVaultAddress: "http://openbao.openbao.svc:8200"}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "https://") {
		t.Fatalf("expected an https-only error, got: %v", err)
	}
}

func TestInstallArgoWorkflowsSurfacesHelmFailure(t *testing.T) {
	t.Parallel()
	deps := Deps{
		Output:     io.Discard,
		KubeClient: fake.NewClientset(),
		RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"},
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if args[0] == "list" {
				return []byte("[]"), nil
			}
			if args[0] == "upgrade" {
				return nil, errors.New("chart pull failed")
			}
			return nil, nil
		},
		PollInterval: time.Millisecond,
	}
	cfg := Config{}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallArgoWorkflows(context.Background(), cfg, deps); err == nil {
		t.Fatal("expected the helm failure to surface")
	}
}

// The local-iteration loop the rollout depends on: the working tree's chart and
// a locally built image, with no published release in between.
func TestOberthHelmArgsUsesLocalChartAndImageWhenGiven(t *testing.T) {
	t.Parallel()
	cfg := Config{ChartPath: "./charts/oberth", ImageRef: "oberth-server:argo-live", ChartVersion: "v0.10.94"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	args := OberthHelmArgs(cfg, OpenBaoResult{}, RekorResult{})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--install oberth ./charts/oberth") {
		t.Fatalf("local chart path not used:\n%s", joined)
	}
	if !strings.Contains(joined, "--set-string image.ref=oberth-server:argo-live") {
		t.Fatalf("image override not applied:\n%s", joined)
	}
	if slices.Contains(args, "--version") {
		t.Fatalf("--version selects a published chart and must not accompany a local path:\n%s", joined)
	}
}

func TestArgoOberthHelmValuesCarriesVaultBinding(t *testing.T) {
	t.Parallel()
	cfg := Config{
		ArgoVaultAddress:          "https://openbao.openbao.svc:8200",
		ArgoVaultCredentialedRole: "oberth-credentialed",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argoOberthHelmValues(cfg, OpenBaoResult{}), " ")
	for _, want := range []string{
		"argo.vault.address=https://openbao.openbao.svc:8200",
		"argo.vault.credentialedRole=oberth-credentialed",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
}

// A default `oberth install --install-secretstore` must wire argo.vault.address
// to the installed store's service address so the first credentialed release
// step can reach OpenBao without an explicit --argo-vault-address flag. The
// CA cert is auto-pinned because the defaulted address matches ServiceAddress.
func TestArgoOberthHelmValuesDefaultsToInstalledStore(t *testing.T) {
	t.Parallel()
	cfg := Config{InstallSecretStore: true}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	openbao := OpenBaoResult{
		ServiceAddress: "https://openbao.openbao.svc:8200",
		CACertPEM:      "-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----",
	}
	values := argoOberthHelmValues(cfg, openbao)
	joined := strings.Join(values, " ")
	if !strings.Contains(joined, "argo.vault.address="+openbao.ServiceAddress) {
		t.Errorf("expected vault address defaulted to installed store, got: %s", joined)
	}
	if !strings.Contains(joined, "argo.vault.caCert=") {
		t.Errorf("expected auto-wired CA cert for the installed store, got: %s", joined)
	}
}

// An explicit --argo-vault-address pointing at a foreign store must be
// respected verbatim — no defaulting, and no CA pinned from a store the
// address does not point at.
func TestArgoOberthHelmValuesRespectsExplicitForeignAddress(t *testing.T) {
	t.Parallel()
	cfg := Config{
		InstallSecretStore: true,
		ArgoVaultAddress:   "https://external-vault.example.com:8200",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	openbao := OpenBaoResult{
		ServiceAddress: "https://openbao.openbao.svc:8200",
		CACertPEM:      "-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----",
	}
	values := argoOberthHelmValues(cfg, openbao)
	joined := strings.Join(values, " ")
	if !strings.Contains(joined, "argo.vault.address=https://external-vault.example.com:8200") {
		t.Errorf("expected explicit foreign address preserved, got: %s", joined)
	}
	if strings.Contains(joined, "argo.vault.caCert=") {
		t.Errorf("CA cert must not be pinned for a foreign store: %s", joined)
	}
}

// Without a secret store, no vault address or CA is set — there is nothing to
// point pipelines at.
func TestArgoOberthHelmValuesOmitsVaultWithoutStore(t *testing.T) {
	t.Parallel()
	cfg := Config{}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	values := argoOberthHelmValues(cfg, OpenBaoResult{})
	joined := strings.Join(values, " ")
	if strings.Contains(joined, "argo.vault.address=") {
		t.Errorf("vault address must not be set without a store: %s", joined)
	}
	if strings.Contains(joined, "argo.vault.caCert=") {
		t.Errorf("CA cert must not be set without a store: %s", joined)
	}
}
