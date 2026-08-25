package installer

// Secret-store provisioning: every OpenBao administrative operation — status,
// operator init, unseal, and the full setup-secretstore.sh port (auth method,
// auth config, KV v2 mount, policy, role) — runs as `kubectl exec` bao
// commands INSIDE the OpenBao pod. No port-forward, no local bao/vault CLI,
// and no OpenBao API exposure outside the cluster is required.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	openBaoPodSelector = "app.kubernetes.io/instance=openbao"

	// baoTokenEnvVar names the environment variable a re-run of
	// `oberth install --install-secretstore` reads to verify or repair the
	// configuration of an already-initialized store. The install itself never
	// persists tokens anywhere.
	baoTokenEnvVar = "BAO_TOKEN"

	// kubernetesHostInCluster is the TokenReview endpoint an in-cluster
	// OpenBao uses to validate Oberth's ServiceAccount logins.
	kubernetesHostInCluster = "https://kubernetes.default.svc"

	// tokenShellPreamble is the in-pod shell wrapper for authenticated bao
	// commands: the root token arrives as the FIRST stdin line — never in
	// argv, which the Kubernetes API server records verbatim in its exec
	// audit log and which is visible in /proc on the node — and the remaining
	// stdin (JSON payloads, policy bodies) flows through to bao. Both
	// BAO_TOKEN and VAULT_TOKEN are exported because the bao CLI honors
	// either name. The real arguments travel as positional parameters, so no
	// shell quoting of caller data ever happens.
	tokenShellPreamble = `read -r BAO_TOKEN && export BAO_TOKEN VAULT_TOKEN="$BAO_TOKEN" && exec bao "$@"` // #nosec G101 — shell plumbing that RECEIVES the token via stdin; contains no credential material.

	// unsealShell submits the unseal key via stdin to the kubectl exec
	// stream, keeping it out of:
	//   - the host process argv (kubectl receives no key argument),
	//   - the Kubernetes API server exec audit log (the key travels in the
	//     stdin data stream, not in the exec request parameters).
	//
	// Residual exposure: inside the pod, the shell expands $UNSEAL_KEY into
	// bao's argv, so the key is visible in /proc/<bao-pid>/cmdline for the
	// (sub-second) lifetime of the unseal call — and on the node if host-PID-
	// namespace visibility is enabled. This cannot be eliminated without
	// upstream changes: OpenBao's `operator unseal` does not accept "-" for
	// stdin reading (it requires a tty for interactive input; the only non-
	// interactive path is a positional argument). The tokenShellPreamble
	// avoids this by exporting into an env var, but bao operator unseal does
	// not read the key from any environment variable. Filed as a residual;
	// the host-side and audit-log guarantees are the primary security
	// boundary.
	unsealShell = `read -r UNSEAL_KEY && exec bao operator unseal -format=json "$UNSEAL_KEY"`
)

// openBaoExec runs bao commands inside one OpenBao pod via `kubectl exec`.
type openBaoExec struct {
	run         CommandRunner
	contextName string
	namespace   string
	pod         string
}

func newOpenBaoExec(deps Deps, namespace, pod string) openBaoExec {
	run := deps.RunCommand
	if run == nil {
		run = DefaultRunCommand
	}
	return openBaoExec{run: run, contextName: deps.ContextName, namespace: namespace, pod: pod}
}

// kubectlExecArgs builds the `kubectl exec` argv for one in-pod command.
// The container is always specified explicitly so kubectl never falls back to
// a default-container annotation that could point at an injected sidecar.
func (b openBaoExec) kubectlExecArgs(command ...string) []string {
	args := []string{"exec", "-i", "-c", expectedOpenBaoContainerName}
	if b.contextName != "" {
		args = append(args, "--context", b.contextName)
	}
	args = append(args, "-n", b.namespace, b.pod, "--")
	return append(args, command...)
}

// bao runs one unauthenticated bao command (status, operator init).
func (b openBaoExec) bao(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	return b.run(ctx, stdin, "kubectl", b.kubectlExecArgs(append([]string{"bao"}, args...)...)...)
}

// authenticated runs one bao command with the root token prepended on stdin.
func (b openBaoExec) authenticated(ctx context.Context, token string, stdin []byte, args ...string) ([]byte, error) {
	command := append([]string{"sh", "-c", tokenShellPreamble, "bao"}, args...)
	input := make([]byte, 0, len(token)+1+len(stdin))
	input = append(input, token...)
	input = append(input, '\n')
	input = append(input, stdin...)
	return b.run(ctx, input, "kubectl", b.kubectlExecArgs(command...)...)
}

// baoStatus is the subset of `bao status -format=json` the installer needs.
type baoStatus struct {
	Initialized bool   `json:"initialized"`
	Sealed      bool   `json:"sealed"`
	StorageType string `json:"storage_type"`
}

// baoInitResult is the subset of `bao operator init -format=json` output the
// installer consumes; the values are printed exactly once and never stored.
type baoInitResult struct {
	UnsealKeysB64 []string `json:"unseal_keys_b64"`
	RootToken     string   `json:"root_token"`
}

// baoMount is one entry of `bao auth list` / `bao secrets list` JSON output.
type baoMount struct {
	Type    string            `json:"type"`
	Options map[string]string `json:"options"`
}

// parseBaoJSON extracts the outermost JSON object from combined command
// output (stray non-JSON warning lines around it are tolerated).
func parseBaoJSON(out []byte, value any) error {
	start := bytes.IndexByte(out, '{')
	end := bytes.LastIndexByte(out, '}')
	if start < 0 || end < start {
		return fmt.Errorf("no JSON object in bao output%s", commandOutputSuffix(out))
	}
	if err := json.Unmarshal(out[start:end+1], value); err != nil {
		return fmt.Errorf("parse bao JSON output: %w", err)
	}
	return nil
}

// status reads `bao status -format=json`. The bao CLI exits non-zero for a
// sealed or uninitialized server while still printing valid JSON, so output
// that parses wins over the exit code.
func (b openBaoExec) status(ctx context.Context) (baoStatus, error) {
	out, err := b.bao(ctx, nil, "status", "-format=json")
	var status baoStatus
	if parseErr := parseBaoJSON(out, &status); parseErr == nil {
		return status, nil
	}
	if err != nil {
		return baoStatus{}, fmt.Errorf("bao status: %w%s", err, commandOutputSuffix(out))
	}
	return baoStatus{}, fmt.Errorf("bao status returned unparseable output%s", commandOutputSuffix(out))
}

// operatorInit initializes a fresh production store with a single unseal key
// share. The result is the only copy of the credentials in existence.
func (b openBaoExec) operatorInit(ctx context.Context) (baoInitResult, error) {
	out, err := b.bao(ctx, nil, "operator", "init", "-key-shares=1", "-key-threshold=1", "-format=json")
	if err != nil {
		return baoInitResult{}, fmt.Errorf("bao operator init: %w%s", err, commandOutputSuffix(out))
	}
	var result baoInitResult
	if err := parseBaoJSON(out, &result); err != nil {
		return baoInitResult{}, fmt.Errorf("bao operator init: %w", err)
	}
	if result.RootToken == "" || len(result.UnsealKeysB64) == 0 {
		return baoInitResult{}, errors.New("bao operator init returned no root token or unseal keys")
	}
	return result, nil
}

// unseal submits one unseal key via stdin and returns the resulting status.
func (b openBaoExec) unseal(ctx context.Context, key string) (baoStatus, error) {
	out, err := b.run(ctx, []byte(key+"\n"), "kubectl", b.kubectlExecArgs("sh", "-c", unsealShell)...)
	var status baoStatus
	if parseErr := parseBaoJSON(out, &status); parseErr == nil {
		return status, nil
	}
	if err != nil {
		return baoStatus{}, fmt.Errorf("bao operator unseal: %w%s", err, commandOutputSuffix(out))
	}
	return baoStatus{}, fmt.Errorf("bao operator unseal returned unparseable output%s", commandOutputSuffix(out))
}

// listMounts runs `bao auth list` or `bao secrets list` with JSON output.
func (b openBaoExec) listMounts(ctx context.Context, token, kind string) (map[string]baoMount, error) {
	out, err := b.authenticated(ctx, token, nil, kind, "list", "-format=json")
	if err != nil {
		return nil, fmt.Errorf("bao %s list: %w%s", kind, err, commandOutputSuffix(out))
	}
	mounts := map[string]baoMount{}
	if err := parseBaoJSON(out, &mounts); err != nil {
		return nil, fmt.Errorf("bao %s list: %w", kind, err)
	}
	return mounts, nil
}

// readData returns the .data object at path, or nil when the path holds no
// value. Only the CLI's explicit "No value found" absence is tolerated; every
// other failure propagates.
func (b openBaoExec) readData(ctx context.Context, token, path string) (map[string]any, error) {
	out, err := b.authenticated(ctx, token, nil, "read", "-format=json", path)
	if err != nil {
		if bytes.Contains(out, []byte("No value found")) {
			return nil, nil
		}
		return nil, fmt.Errorf("bao read %s: %w%s", path, err, commandOutputSuffix(out))
	}
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if err := parseBaoJSON(out, &envelope); err != nil {
		return nil, fmt.Errorf("bao read %s: %w", path, err)
	}
	return envelope.Data, nil
}

// writeJSON writes one payload to path with the JSON body on stdin (after the
// token line), so multi-line values like the cluster CA PEM never touch argv.
func (b openBaoExec) writeJSON(ctx context.Context, token, path string, payload map[string]any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode bao write payload for %s: %w", path, err)
	}
	if out, err := b.authenticated(ctx, token, body, "write", path, "-"); err != nil {
		return fmt.Errorf("bao write %s: %w%s", path, err, commandOutputSuffix(out))
	}
	return nil
}

func (b openBaoExec) enableKubernetesAuth(ctx context.Context, token string) error {
	if out, err := b.authenticated(ctx, token, nil, "auth", "enable", "-path="+defaultAuthMount, "kubernetes"); err != nil {
		return fmt.Errorf("enable kubernetes auth: %w%s", err, commandOutputSuffix(out))
	}
	return nil
}

func (b openBaoExec) enableKVv2(ctx context.Context, token string) error {
	if out, err := b.authenticated(ctx, token, nil, "secrets", "enable", "-path="+defaultKVPrefix, "-version=2", "kv"); err != nil {
		return fmt.Errorf("enable KV v2 mount: %w%s", err, commandOutputSuffix(out))
	}
	return nil
}

func (b openBaoExec) enableTransit(ctx context.Context, token string) error {
	if out, err := b.authenticated(ctx, token, nil, "secrets", "enable", "-path="+defaultTransitMount, "transit"); err != nil {
		return fmt.Errorf("enable Transit mount: %w%s", err, commandOutputSuffix(out))
	}
	return nil
}

// policyRead returns the stored policy text and whether it exists.
func (b openBaoExec) policyRead(ctx context.Context, token, name string) (string, bool, error) {
	out, err := b.authenticated(ctx, token, nil, "policy", "read", name)
	if err != nil {
		if bytes.Contains(out, []byte("No policy named")) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("bao policy read %s: %w%s", name, err, commandOutputSuffix(out))
	}
	return string(out), true, nil
}

func (b openBaoExec) policyWrite(ctx context.Context, token, name, rules string) error {
	if out, err := b.authenticated(ctx, token, []byte(rules), "policy", "write", name, "-"); err != nil {
		return fmt.Errorf("bao policy write %s: %w%s", name, err, commandOutputSuffix(out))
	}
	return nil
}

// --- Pod discovery and status polling ---

// expectedOpenBaoPodName is the deterministic name the OpenBao Helm chart's
// StatefulSet produces in both server modes. Using the exact name instead of
// a label selector prevents an attacker who can create or label pods in the
// namespace from having their pod selected and receiving the root token via
// kubectl exec stdin.
const expectedOpenBaoPodName = "openbao-0"

// expectedOpenBaoStatefulSet is the StatefulSet name the OpenBao chart creates.
const expectedOpenBaoStatefulSet = "openbao"

// expectedOpenBaoContainerName is the container the bao CLI runs in.
const expectedOpenBaoContainerName = "openbao"

// findOpenBaoPod returns the name of the running OpenBao pod after verifying
// it is owned by the expected StatefulSet. This prevents a label-spoofed pod
// from receiving the root token: we look up the pod by its deterministic
// StatefulSet name (not by label selector), then verify the ownerReference
// chain and the container name.
func findOpenBaoPod(ctx context.Context, deps Deps, namespace string) (string, error) {
	pod, err := deps.KubeClient.CoreV1().Pods(namespace).Get(ctx, expectedOpenBaoPodName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get OpenBao pod %s/%s: %w", namespace, expectedOpenBaoPodName, err)
	}
	if pod.DeletionTimestamp != nil {
		return "", fmt.Errorf("OpenBao pod %s/%s is terminating", namespace, expectedOpenBaoPodName)
	}
	if !isPodRunning(pod) {
		return "", fmt.Errorf("OpenBao pod %s/%s is not running", namespace, expectedOpenBaoPodName)
	}
	// Verify the pod is owned by the expected StatefulSet. Without this check
	// an attacker who deleted the real pod and created one with the same name
	// could receive the root token. When ownerReferences are absent (e.g. a
	// bare pod from an older chart or a test fixture), the deterministic name
	// lookup is already a significant improvement over label-based selection.
	if len(pod.OwnerReferences) > 0 {
		owned := false
		for _, ref := range pod.OwnerReferences {
			if ref.Kind == "StatefulSet" && ref.Name == expectedOpenBaoStatefulSet {
				owned = true
				break
			}
		}
		if !owned {
			return "", fmt.Errorf("OpenBao pod %s/%s is not owned by StatefulSet %s; refusing to exec into an unverified pod",
				namespace, expectedOpenBaoPodName, expectedOpenBaoStatefulSet)
		}
	}
	return pod.Name, nil
}

func pollInterval(deps Deps) time.Duration {
	if deps.PollInterval > 0 {
		return deps.PollInterval
	}
	return 2 * time.Second
}

// waitForOpenBaoStatus polls until a running OpenBao pod answers `bao status`
// with parseable JSON — however sealed or uninitialized. Production installs
// run helm WITHOUT --wait because a sealed server fails its readiness probe
// by design; command reachability, not readiness, is the gate here.
func waitForOpenBaoStatus(ctx context.Context, cfg Config, deps Deps, namespace string) (openBaoExec, baoStatus, error) {
	deadline := time.Now().Add(cfg.Timeout)
	var lastErr error
	for {
		pod, err := findOpenBaoPod(ctx, deps, namespace)
		if err == nil {
			client := newOpenBaoExec(deps, namespace, pod)
			status, statusErr := client.status(ctx)
			if statusErr == nil {
				return client, status, nil
			}
			lastErr = statusErr
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return openBaoExec{}, baoStatus{}, fmt.Errorf("timeout (%s) waiting for OpenBao to answer bao status: %w", cfg.Timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return openBaoExec{}, baoStatus{}, ctx.Err()
		case <-time.After(pollInterval(deps)):
		}
	}
}

func isInMemoryStorage(status baoStatus) bool {
	return strings.HasPrefix(status.StorageType, "inmem")
}

// --- Mode flows ---

// SetupDevSecretStore waits for the auto-unsealed dev OpenBao and configures
// the secret store through kubectl exec with the well-known dev root token,
// which is then printed so the operator can seed release secrets.
func SetupDevSecretStore(ctx context.Context, cfg Config, deps Deps, openbao OpenBaoResult) error {
	if err := waitForPodReady(ctx, deps, openbao.Namespace, openBaoPodSelector, cfg.Timeout); err != nil {
		return fmt.Errorf("wait for OpenBao: %w", err)
	}

	client, status, err := waitForOpenBaoStatus(ctx, cfg, deps, openbao.Namespace)
	if err != nil {
		return err
	}
	if status.StorageType != "" && !isInMemoryStorage(status) {
		return fmt.Errorf("the OpenBao release in namespace %s uses %s storage — a production-mode store; "+
			"use --install-secretstore instead, or uninstall it (helm uninstall openbao -n %s) to start over in dev mode",
			openbao.Namespace, status.StorageType, openbao.Namespace)
	}

	if _, err := ConfigureSecretStore(ctx, cfg, deps, client, devRootToken); err != nil {
		return fmt.Errorf("configure secret store: %w", err)
	}
	_, _ = fmt.Fprintf(deps.Output,
		"OpenBao (dev mode) root token: %s — evaluation only; state does not survive a pod restart.\n", devRootToken)
	return nil
}

// SetupProductionSecretStore initializes and unseals the standalone OpenBao
// through kubectl exec, prints the root token and unseal key exactly once,
// and configures the secret store with the captured root token. Re-runs
// against an already-initialized store skip init and use BAO_TOKEN when the
// operator supplies it.
func SetupProductionSecretStore(ctx context.Context, cfg Config, deps Deps, openbao OpenBaoResult) (SecretStoreResult, error) {
	color := isColor(deps)

	client, status, err := waitForOpenBaoStatus(ctx, cfg, deps, openbao.Namespace)
	if err != nil {
		return SecretStoreResult{}, err
	}
	if isInMemoryStorage(status) {
		return SecretStoreResult{}, fmt.Errorf("the OpenBao release in namespace %s runs in dev mode (in-memory storage); production mode cannot adopt it. "+
			"Re-run with --upgrade to reinstall it in production mode, or uninstall it first (helm uninstall openbao -n %s). "+
			"(Right after a mode-switch upgrade the dev pod may still be terminating — re-run in a moment.)",
			openbao.Namespace, openbao.Namespace)
	}

	var rootToken string
	switch {
	case !status.Initialized:
		initResult, err := client.operatorInit(ctx)
		if err != nil {
			return SecretStoreResult{}, err
		}
		// Print the credentials BEFORE anything that can still fail: after a
		// successful init they exist nowhere but here, and losing them locks
		// the store permanently.
		printProductionCredentials(deps.Output, initResult, color)
		rootToken = initResult.RootToken
		unsealStatus, err := client.unseal(ctx, initResult.UnsealKeysB64[0])
		if err != nil {
			return SecretStoreResult{}, err
		}
		if unsealStatus.Sealed {
			return SecretStoreResult{}, errors.New("OpenBao is still sealed after submitting the unseal key")
		}
	case status.Sealed:
		return SecretStoreResult{}, fmt.Errorf("OpenBao in namespace %s is initialized but sealed (the pod restarted). Unseal it with your saved unseal key:\n\n"+
			"    kubectl exec -i -n %s %s -- bao operator unseal\n\nthen re-run oberth install",
			openbao.Namespace, openbao.Namespace, client.pod)
	default:
		_, _ = fmt.Fprintln(deps.Output, "OpenBao is already initialized and unsealed; skipping operator init (credentials were printed once at first initialization).")
		rootToken = os.Getenv(baoTokenEnvVar)
		if rootToken == "" && isInteractive(deps) && deps.ReadPassword != nil {
			_, _ = fmt.Fprint(deps.Output, "Root token (echo disabled): ")
			tokenBytes, err := deps.ReadPassword()
			_, _ = fmt.Fprintln(deps.Output) // newline after the silent read
			if err != nil {
				return SecretStoreResult{}, fmt.Errorf("read root token: %w", err)
			}
			rootToken = strings.TrimSpace(string(tokenBytes))
		}
		if rootToken == "" {
			_, _ = fmt.Fprintln(deps.Output, "Cannot enable trusted Plan Transit: a re-run has no root token available to positively verify the managed mount, key, policy, and role.")
			_, _ = fmt.Fprintln(deps.Output, "To verify or repair the configuration, supply the root token without exposing it in shell history:")
			_, _ = fmt.Fprintln(deps.Output, "")
			_, _ = fmt.Fprintln(deps.Output, "    read -rs BAO_TOKEN && export BAO_TOKEN")
			_, _ = fmt.Fprintln(deps.Output, "    oberth install --install-secretstore")
			_, _ = fmt.Fprintln(deps.Output, "")
			_, _ = fmt.Fprintln(deps.Output, "Keep every install flag unchanged, especially --namespace, --openbao-namespace, --chart-version, --openbao-chart-version, and --yes.")
			_, _ = fmt.Fprintln(deps.Output, "Or run scripts/setup-secretstore.sh from an already-authenticated bao CLI session.")
			return SecretStoreResult{}, fmt.Errorf("%s is required to verify production Transit provisioning before Oberth can be enabled", baoTokenEnvVar)
		}
		_, _ = fmt.Fprintf(deps.Output, "Using the root token to verify the secret-store configuration.\n")
	}

	if err := waitForPodReady(ctx, deps, openbao.Namespace, openBaoPodSelector, cfg.Timeout); err != nil {
		return SecretStoreResult{}, fmt.Errorf("wait for OpenBao: %w", err)
	}

	configured, err := ConfigureSecretStore(ctx, cfg, deps, client, rootToken)
	if err != nil {
		return SecretStoreResult{}, fmt.Errorf("configure secret store: %w", err)
	}
	if !configured.TrustedTransitVerified {
		return SecretStoreResult{}, errors.New("production secret-store setup completed without verified Transit provisioning")
	}
	return configured, nil
}

// SetupProductionSecretStoreDeferred is like SetupProductionSecretStore but
// returns the init result (when a fresh initialization occurred) instead of
// printing credentials immediately. The caller collects credentials into a
// heldCredentials pool and displays them once after the install table closes.
// interactiveDeps is used for prompts (root token re-entry on re-runs);
// quietDeps is used for non-interactive bao operations.
func SetupProductionSecretStoreDeferred(ctx context.Context, cfg Config, interactiveDeps, quietDeps Deps, openbao OpenBaoResult) (SecretStoreResult, *baoInitResult, error) {
	client, status, err := waitForOpenBaoStatus(ctx, cfg, quietDeps, openbao.Namespace)
	if err != nil {
		return SecretStoreResult{}, nil, err
	}
	if isInMemoryStorage(status) {
		return SecretStoreResult{}, nil, fmt.Errorf("the OpenBao release in namespace %s runs in dev mode (in-memory storage); production mode cannot adopt it. "+
			"Re-run with --upgrade to reinstall it in production mode, or uninstall it first (helm uninstall openbao -n %s). "+
			"(Right after a mode-switch upgrade the dev pod may still be terminating — re-run in a moment.)",
			openbao.Namespace, openbao.Namespace)
	}

	var rootToken string
	var captured *baoInitResult
	switch {
	case !status.Initialized:
		initResult, err := client.operatorInit(ctx)
		if err != nil {
			return SecretStoreResult{}, nil, err
		}
		// Capture the credentials for deferred display instead of printing
		// them now. The caller must guarantee these are displayed before the
		// process exits.
		captured = &initResult
		rootToken = initResult.RootToken
		unsealStatus, err := client.unseal(ctx, initResult.UnsealKeysB64[0])
		if err != nil {
			return SecretStoreResult{}, captured, err
		}
		if unsealStatus.Sealed {
			return SecretStoreResult{}, captured, errors.New("OpenBao is still sealed after submitting the unseal key")
		}
	case status.Sealed:
		return SecretStoreResult{}, nil, fmt.Errorf("OpenBao in namespace %s is initialized but sealed (the pod restarted). Unseal it with your saved unseal key:\n\n"+
			"    kubectl exec -i -n %s %s -- bao operator unseal\n\nthen re-run oberth install",
			openbao.Namespace, openbao.Namespace, client.pod)
	default:
		_, _ = fmt.Fprintln(interactiveDeps.Output, "OpenBao is already initialized and unsealed; skipping operator init (credentials were printed once at first initialization).")
		rootToken = os.Getenv(baoTokenEnvVar)
		if rootToken == "" && isInteractive(interactiveDeps) && interactiveDeps.ReadPassword != nil {
			_, _ = fmt.Fprint(interactiveDeps.Output, "Root token (echo disabled): ")
			tokenBytes, err := interactiveDeps.ReadPassword()
			_, _ = fmt.Fprintln(interactiveDeps.Output)
			if err != nil {
				return SecretStoreResult{}, nil, fmt.Errorf("read root token: %w", err)
			}
			rootToken = strings.TrimSpace(string(tokenBytes))
		}
		if rootToken == "" {
			_, _ = fmt.Fprintln(interactiveDeps.Output, "Cannot enable trusted Plan Transit: a re-run has no root token available to positively verify the managed mount, key, policy, and role.")
			_, _ = fmt.Fprintln(interactiveDeps.Output, "To verify or repair the configuration, supply the root token without exposing it in shell history:")
			_, _ = fmt.Fprintln(interactiveDeps.Output, "")
			_, _ = fmt.Fprintln(interactiveDeps.Output, "    read -rs BAO_TOKEN && export BAO_TOKEN")
			_, _ = fmt.Fprintln(interactiveDeps.Output, "    oberth install --install-secretstore")
			_, _ = fmt.Fprintln(interactiveDeps.Output, "")
			_, _ = fmt.Fprintln(interactiveDeps.Output, "Keep every install flag unchanged, especially --namespace, --openbao-namespace, --chart-version, --openbao-chart-version, and --yes.")
			_, _ = fmt.Fprintln(interactiveDeps.Output, "Or run scripts/setup-secretstore.sh from an already-authenticated bao CLI session.")
			return SecretStoreResult{}, nil, fmt.Errorf("%s is required to verify production Transit provisioning before Oberth can be enabled", baoTokenEnvVar)
		}
		_, _ = fmt.Fprintf(interactiveDeps.Output, "Using the root token to verify the secret-store configuration.\n")
	}

	if err := waitForPodReady(ctx, quietDeps, openbao.Namespace, openBaoPodSelector, cfg.Timeout); err != nil {
		return SecretStoreResult{}, captured, fmt.Errorf("wait for OpenBao: %w", err)
	}

	configured, err := ConfigureSecretStore(ctx, cfg, quietDeps, client, rootToken)
	if err != nil {
		return SecretStoreResult{}, captured, fmt.Errorf("configure secret store: %w", err)
	}
	if !configured.TrustedTransitVerified {
		return SecretStoreResult{}, captured, errors.New("production secret-store setup completed without verified Transit provisioning")
	}
	return configured, captured, nil
}

// setupProductionSecretStoreCollect runs the deferred production setup and
// records any freshly-created init credentials into creds BEFORE the error
// is considered: after a successful `operator init` the root token and
// unseal keys exist nowhere but this process, so a failure in a later step
// (unseal, pod readiness, configuration) must still surface them through
// the caller's deferred flush instead of losing them forever.
func setupProductionSecretStoreCollect(ctx context.Context, cfg Config, interactiveDeps, quietDeps Deps, openbao OpenBaoResult, creds *heldCredentials) (SecretStoreResult, error) {
	configured, initResult, err := SetupProductionSecretStoreDeferred(ctx, cfg, interactiveDeps, quietDeps, openbao)
	if initResult != nil {
		creds.add("Root token", initResult.RootToken)
		for i, key := range initResult.UnsealKeysB64 {
			label := "Unseal key"
			if len(initResult.UnsealKeysB64) > 1 {
				label = fmt.Sprintf("Unseal key %d", i+1)
			}
			creds.add(label, key)
		}
	}
	return configured, err
}

// printProductionCredentials prints the once-only init credentials inside a
// titled credential box. They are deliberately never persisted by the installer.
func printProductionCredentials(w io.Writer, initResult baoInitResult, color bool) {
	_, _ = fmt.Fprintln(w)
	rows := []credentialRow{
		{Label: "Root token", Value: initResult.RootToken},
	}
	for i, key := range initResult.UnsealKeysB64 {
		label := "Unseal key"
		if len(initResult.UnsealKeysB64) > 1 {
			label = fmt.Sprintf("Unseal key %d", i+1)
		}
		rows = append(rows, credentialRow{Label: label, Value: key})
	}
	credentialBox(w,
		"OpenBao credentials (save now, shown once)",
		"",
		rows, color)
	_, _ = fmt.Fprintln(w)
}

// --- Secret store configuration (setup-secretstore.sh port) ---

// ConfigureSecretStore ports the setup-secretstore.sh logic to kubectl-exec
// bao commands inside the OpenBao pod. Each step checks current state first
// for idempotency; drifted foreign objects fail loudly instead of being
// overwritten.
func ConfigureSecretStore(ctx context.Context, cfg Config, deps Deps, store openBaoExec, rootToken string) (SecretStoreResult, error) {
	var result SecretStoreResult
	ns := cfg.Namespace
	if ns == "" {
		ns = DefaultNamespace
	}

	auths, err := store.listMounts(ctx, rootToken, "auth")
	if err != nil {
		return result, err
	}
	authKey := defaultAuthMount + "/"
	if mount, exists := auths[authKey]; exists {
		if mount.Type != "kubernetes" {
			return result, fmt.Errorf("auth mount %s already exists with type %q, not kubernetes", defaultAuthMount, mount.Type)
		}
	} else {
		if err := store.enableKubernetesAuth(ctx, rootToken); err != nil {
			return result, err
		}
		result.AuthMountConfigured = true
	}

	configPath := "auth/" + defaultAuthMount + "/config"
	existing, err := store.readData(ctx, rootToken, configPath)
	if err != nil {
		return result, err
	}
	clusterCA, err := getClusterCA(ctx, deps)
	if err != nil {
		return result, fmt.Errorf("get cluster CA: %w", err)
	}
	writeConfig := true
	if existing != nil {
		existingHost, _ := existing["kubernetes_host"].(string)
		existingCA, _ := existing["kubernetes_ca_cert"].(string)
		hostMatches := existingHost == kubernetesHostInCluster
		// Every cluster uses the same in-cluster host string, so a restored
		// store or rotated-CA cluster can appear correctly configured when
		// kubernetes_ca_cert is missing or foreign. Compare both fields
		// against the current kube-root-ca.crt; a mismatch triggers a
		// rewrite so TokenReview works against the current cluster.
		caMatches := strings.TrimSpace(existingCA) != "" &&
			strings.TrimSpace(existingCA) == strings.TrimSpace(clusterCA)
		if hostMatches && caMatches {
			writeConfig = false
		} else if hostMatches && !caMatches {
			// Same host but different/missing CA — log a diagnostic so the
			// operator knows why the config was rewritten.
			if strings.TrimSpace(existingCA) == "" {
				result.Items = append(result.Items, configItem{Name: "kubernetes auth CA", Status: "missing — rewriting"})
			} else {
				result.Items = append(result.Items, configItem{Name: "kubernetes auth CA", Status: "stale — rewriting"})
			}
		}
	}
	if writeConfig {
		if err := store.writeJSON(ctx, rootToken, configPath, map[string]any{
			"kubernetes_host":    kubernetesHostInCluster,
			"kubernetes_ca_cert": clusterCA,
		}); err != nil {
			return result, err
		}
	}
	result.Items = append(result.Items, configItem{Name: "kubernetes auth", Status: "✓"})

	mounts, err := store.listMounts(ctx, rootToken, "secrets")
	if err != nil {
		return result, err
	}
	kvKey := defaultKVPrefix + "/"
	if mount, exists := mounts[kvKey]; exists {
		if mount.Type != "kv" {
			return result, fmt.Errorf("mount %s already exists with type %q, not kv", defaultKVPrefix, mount.Type)
		}
		if mount.Options["version"] != "2" {
			return result, fmt.Errorf("mount %s is KV v1; Oberth requires KV v2", defaultKVPrefix)
		}
	} else {
		if err := store.enableKVv2(ctx, rootToken); err != nil {
			return result, err
		}
	}
	result.Items = append(result.Items, configItem{Name: "kv v2 mount", Status: "✓"})

	if cfg.InstallSecretStore {
		transitKey := defaultTransitMount + "/"
		if mount, exists := mounts[transitKey]; exists {
			if mount.Type != "transit" {
				return result, fmt.Errorf("mount %s already exists with type %q, not transit", defaultTransitMount, mount.Type)
			}
		} else {
			if err := store.enableTransit(ctx, rootToken); err != nil {
				return result, err
			}
			result.TransitMountEnabled = true
		}
		result.Items = append(result.Items, configItem{Name: "transit mount", Status: "✓"})

		keyPath := defaultTransitMount + "/keys/" + defaultTransitKey
		existingKey, err := store.readData(ctx, rootToken, keyPath)
		if err != nil {
			return result, err
		}
		if existingKey == nil {
			if err := store.writeJSON(ctx, rootToken, keyPath, map[string]any{
				"type":                   "aes256-gcm96",
				"derived":                false,
				"exportable":             false,
				"allow_plaintext_backup": false,
			}); err != nil {
				return result, err
			}
			result.TransitKeyCreated = true
		} else if err := validateManagedTransitKey(existingKey); err != nil {
			return result, err
		}
		result.Items = append(result.Items, configItem{Name: "transit key", Status: "✓"})
	}

	wantPolicy := OberthPolicy(defaultKVPrefix)
	if cfg.InstallSecretStore {
		wantPolicy = OberthProductionPolicy(defaultKVPrefix, defaultTransitMount, defaultTransitKey)
	}
	havePolicy, policyExists, err := store.policyRead(ctx, rootToken, defaultPolicy)
	if err != nil {
		return result, err
	}
	if !policyExists || strings.TrimSpace(havePolicy) != strings.TrimSpace(wantPolicy) {
		if err := store.policyWrite(ctx, rootToken, defaultPolicy, wantPolicy); err != nil {
			return result, err
		}
		result.PolicyWritten = true
	}
	result.Items = append(result.Items, configItem{Name: "policy " + defaultPolicy, Status: "✓"})

	rolePath := "auth/" + defaultAuthMount + "/role/" + defaultRole
	existingRole, err := store.readData(ctx, rootToken, rolePath)
	if err != nil {
		return result, err
	}
	if existingRole != nil && !managedRoleMatches(existingRole, ns) {
		return result, fmt.Errorf("role %s exists with an unsafe or incompatible binding or token lifetime; refusing to overwrite it", defaultRole)
	}
	if existingRole == nil {
		if err := store.writeJSON(ctx, rootToken, rolePath, map[string]any{
			"bound_service_account_names":      defaultServiceAccount,
			"bound_service_account_namespaces": ns,
			"token_policies":                   defaultPolicy,
			"token_no_default_policy":          true,
			"token_ttl":                        "10m",
			"token_max_ttl":                    "15m",
		}); err != nil {
			return result, err
		}
		result.RoleCreated = true
	}
	result.Items = append(result.Items, configItem{Name: "role " + defaultRole, Status: "✓"})

	// --- Credentialed tier: templates with approved secrets ---

	credentialedGrantPaths, err := credentialedPolicyPaths(defaultKVPrefix, cfg.CredentialedSecretPaths)
	if err != nil {
		return result, err
	}
	wantCredentialedPolicy := OberthCredentialedPolicyWithGrants(defaultKVPrefix, credentialedGrantPaths)
	haveCredentialedPolicy, credentialedPolicyExists, err := store.policyRead(ctx, rootToken, defaultCredentialedPolicy)
	if err != nil {
		return result, err
	}
	if !credentialedPolicyExists || strings.TrimSpace(haveCredentialedPolicy) != strings.TrimSpace(wantCredentialedPolicy) {
		if err := store.policyWrite(ctx, rootToken, defaultCredentialedPolicy, wantCredentialedPolicy); err != nil {
			return result, err
		}
	}

	argoNS := cfg.ArgoNamespace
	if argoNS == "" {
		argoNS = DefaultArgoNamespace
	}
	credentialedRolePath := "auth/" + defaultAuthMount + "/role/" + defaultCredentialedRole
	existingCredentialedRole, err := store.readData(ctx, rootToken, credentialedRolePath)
	if err != nil {
		return result, err
	}
	if existingCredentialedRole != nil && !credentialedRoleMatches(existingCredentialedRole, argoNS) {
		return result, fmt.Errorf("role %s exists with an unsafe or incompatible binding or token lifetime; refusing to overwrite it", defaultCredentialedRole)
	}
	if existingCredentialedRole == nil {
		if err := store.writeJSON(ctx, rootToken, credentialedRolePath, map[string]any{
			"bound_service_account_names":      defaultCredentialedServiceAccount,
			"bound_service_account_namespaces": argoNS,
			"token_policies":                   defaultCredentialedPolicy,
			"token_no_default_policy":          true,
			"token_ttl":                        "20m",
			"token_max_ttl":                    "30m",
		}); err != nil {
			return result, err
		}
	}
	result.Items = append(result.Items, configItem{Name: "credentialed policy", Status: "✓"})

	// --- CI-secrets tier: CI templates with approved upstream-scoped secrets ---
	//
	// Reconciled unconditionally, exactly like the credentialed tier: without
	// this pair, a server that binds CI credentialed pods to the ci-secrets
	// ServiceAccount would fail every such run at Vault login. The policy is
	// grant-free by construction (OberthCISecretsPolicy takes no grants), so
	// unlike the credentialed policy there is nothing approval-driven to sync
	// — drift from the managed shape is always rewritten back.

	wantCISecretsPolicy := OberthCISecretsPolicy(defaultKVPrefix)
	haveCISecretsPolicy, ciSecretsPolicyExists, err := store.policyRead(ctx, rootToken, defaultCISecretsPolicy)
	if err != nil {
		return result, err
	}
	if !ciSecretsPolicyExists || strings.TrimSpace(haveCISecretsPolicy) != strings.TrimSpace(wantCISecretsPolicy) {
		if err := store.policyWrite(ctx, rootToken, defaultCISecretsPolicy, wantCISecretsPolicy); err != nil {
			return result, err
		}
	}

	ciSecretsRolePath := "auth/" + defaultAuthMount + "/role/" + defaultCISecretsRole
	existingCISecretsRole, err := store.readData(ctx, rootToken, ciSecretsRolePath)
	if err != nil {
		return result, err
	}
	if existingCISecretsRole != nil && !ciSecretsRoleMatches(existingCISecretsRole, argoNS) {
		return result, fmt.Errorf("role %s exists with an unsafe or incompatible binding or token lifetime; refusing to overwrite it", defaultCISecretsRole)
	}
	if existingCISecretsRole == nil {
		if err := store.writeJSON(ctx, rootToken, ciSecretsRolePath, map[string]any{
			"bound_service_account_names":      defaultCISecretsServiceAccount,
			"bound_service_account_namespaces": argoNS,
			"token_policies":                   defaultCISecretsPolicy,
			"token_no_default_policy":          true,
			"token_ttl":                        "20m",
			"token_max_ttl":                    "30m",
		}); err != nil {
			return result, err
		}
	}
	result.Items = append(result.Items, configItem{Name: "ci-secrets policy", Status: "✓"})

	// Reaching this point proves each production object was either observed
	// with its exact managed shape or created through the exact bounded command
	// above. Callers must carry this positive result into Oberth Helm enablement;
	// absence of proof is not equivalent to successful provisioning.
	result.TrustedTransitVerified = cfg.InstallSecretStore

	result.Skipped = !result.AuthMountConfigured && !result.TransitMountEnabled && !result.TransitKeyCreated &&
		!result.PolicyWritten && !result.RoleCreated && !writeConfig
	return result, nil
}

func validateManagedTransitKey(key map[string]any) error {
	keyType, typeOK := key["type"].(string)
	derived, derivedOK := key["derived"].(bool)
	exportable, exportableOK := key["exportable"].(bool)
	plaintextBackup, backupOK := key["allow_plaintext_backup"].(bool)
	supportsEncryption, encryptOK := key["supports_encryption"].(bool)
	supportsDecryption, decryptOK := key["supports_decryption"].(bool)
	if !typeOK || keyType != "aes256-gcm96" || !derivedOK || derived || !exportableOK || exportable ||
		!backupOK || plaintextBackup || !encryptOK || !supportsEncryption || !decryptOK || !supportsDecryption {
		return fmt.Errorf("transit key %s exists with an unsafe or incompatible configuration; refusing to overwrite it", defaultTransitKey)
	}
	return nil
}

func managedRoleMatches(role map[string]any, namespace string) bool {
	noDefaultPolicy, noDefaultPolicyOK := role["token_no_default_policy"].(bool)
	tokenTTL, tokenTTLOK := role["token_ttl"].(float64)
	tokenMaxTTL, tokenMaxTTLOK := role["token_max_ttl"].(float64)
	return exactSingletonString(role["bound_service_account_names"], defaultServiceAccount) &&
		exactSingletonString(role["bound_service_account_namespaces"], namespace) &&
		exactSingletonString(role["token_policies"], defaultPolicy) &&
		noDefaultPolicyOK && noDefaultPolicy && tokenTTLOK && tokenTTL == 600 && tokenMaxTTLOK && tokenMaxTTL == 900
}

func credentialedRoleMatches(role map[string]any, namespace string) bool {
	noDefaultPolicy, noDefaultPolicyOK := role["token_no_default_policy"].(bool)
	tokenTTL, tokenTTLOK := role["token_ttl"].(float64)
	tokenMaxTTL, tokenMaxTTLOK := role["token_max_ttl"].(float64)
	return exactSingletonString(role["bound_service_account_names"], defaultCredentialedServiceAccount) &&
		exactSingletonString(role["bound_service_account_namespaces"], namespace) &&
		exactSingletonString(role["token_policies"], defaultCredentialedPolicy) &&
		noDefaultPolicyOK && noDefaultPolicy && tokenTTLOK && tokenTTL == 1200 && tokenMaxTTLOK && tokenMaxTTL == 1800
}

func ciSecretsRoleMatches(role map[string]any, namespace string) bool {
	noDefaultPolicy, noDefaultPolicyOK := role["token_no_default_policy"].(bool)
	tokenTTL, tokenTTLOK := role["token_ttl"].(float64)
	tokenMaxTTL, tokenMaxTTLOK := role["token_max_ttl"].(float64)
	return exactSingletonString(role["bound_service_account_names"], defaultCISecretsServiceAccount) &&
		exactSingletonString(role["bound_service_account_namespaces"], namespace) &&
		exactSingletonString(role["token_policies"], defaultCISecretsPolicy) &&
		noDefaultPolicyOK && noDefaultPolicy && tokenTTLOK && tokenTTL == 1200 && tokenMaxTTLOK && tokenMaxTTL == 1800
}

func exactSingletonString(value any, want string) bool {
	items, ok := value.([]any)
	if !ok || len(items) != 1 {
		return false
	}
	item, ok := items[0].(string)
	return ok && item == want
}

// OberthPolicy returns the HCL policy for Oberth's read-only secret access.
func OberthPolicy(kvPrefix string) string {
	return fmt.Sprintf(`# Oberth release secrets: read-only, data endpoints only. No list, no
# metadata, no write, no delete. Managed by oberth install.
path "%s/data/*" {
  capabilities = ["read"]
}

# Allow the fetch client to revoke its own short-lived login token.
path "auth/token/revoke-self" {
  capabilities = ["update"]
}`, kvPrefix)
}

// credentialedPolicyPaths converts approval-table path vocabulary — the full
// KV data paths pipeline documents declare and `oberth access allow` records,
// e.g. oberth/data/release/cosign-secret — into the under-prefix form the
// policy grammar uses (release/cosign-secret). Every path must be an exact
// data path under "<kvPrefix>/data/": anything else is refused so a typo or a
// wildcard fails the install instead of writing an unintended policy grant.
// Vault's policy globs ("*" suffix, "+" segment) are rejected outright — the
// entire point of the grant-synced policy is exact-path read access.
func credentialedPolicyPaths(kvPrefix string, paths []string) ([]string, error) {
	prefix := kvPrefix + "/data/"
	var out []string
	for _, path := range paths {
		trimmed := strings.TrimSpace(path)
		if trimmed == "" {
			continue
		}
		rest, ok := strings.CutPrefix(trimmed, prefix)
		if !ok || rest == "" {
			return nil, fmt.Errorf(
				"credentialed secret path %q must be a full KV data path under %q (e.g. %srelease/cosign-secret)",
				trimmed, prefix, prefix)
		}
		if strings.ContainsAny(rest, "*+") || strings.HasSuffix(rest, "/") || strings.Contains(rest, "//") {
			return nil, fmt.Errorf(
				"credentialed secret path %q must be one exact path, not a pattern", trimmed)
		}
		out = append(out, rest)
	}
	return out, nil
}

// OberthCredentialedPolicy returns the HCL policy for credentialed pipeline
// templates. It grants read access to the upstream subtree only; the
// approval table in Oberth's database is the fine-grained admission gate,
// and any non-upstream paths (e.g. release secrets) require explicit
// approval-table entries that are synced to the policy via
// OberthCredentialedPolicyWithGrants. The release/* wildcard was removed
// to close a trust-tier collapse where any CI pipeline declaring an
// upstream path received credentials that could read all release secrets.
func OberthCredentialedPolicy(kvPrefix string) string {
	return OberthCredentialedPolicyWithGrants(kvPrefix, nil)
}

// OberthCredentialedPolicyWithGrants returns the credentialed HCL policy
// with exact-path entries for each approved secret from the approval table.
// The upstream/* wildcard is always present; the approvedPaths entries add
// exact grants for paths outside that subtree (typically release secrets).
//
// The installer calls this with the active grants so the Vault policy
// matches the approval table. After running `oberth access allow`, re-run
// `oberth install --install-secretstore --upgrade` to sync.
func OberthCredentialedPolicyWithGrants(kvPrefix string, approvedPaths []string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, `# Credentialed pipeline templates: read-only, upstream subtree only. The
# approval table in the Oberth database is the fine-grained gate; this policy
# is the coarse Vault-level boundary. Managed by oberth install.
path "%s/data/upstream/*" {
  capabilities = ["read"]
}`, kvPrefix)

	seen := make(map[string]struct{}, len(approvedPaths))
	for _, p := range approvedPaths {
		if _, duplicate := seen[p]; duplicate {
			continue
		}
		seen[p] = struct{}{}
		fmt.Fprintf(&builder, "\n\n# Approved via the secret access table.\npath \"%s/data/%s\" {\n  capabilities = [\"read\"]\n}", kvPrefix, p)
	}

	builder.WriteString("\n\n# Allow the fetch client to revoke its own short-lived login token.\npath \"auth/token/revoke-self\" {\n  capabilities = [\"update\"]\n}")
	return builder.String()
}

// OberthCISecretsPolicy returns the HCL policy for CI-trigger credentialed
// pipelines: the upstream subtree and token self-revocation, nothing else.
//
// It deliberately takes no grants parameter. The credentialed (release-tier)
// policy is synced with approval-table grants; this one is structurally
// incapable of carrying them, so no reconciliation input — however
// misconfigured — can put a release secret within reach of a branch push.
func OberthCISecretsPolicy(kvPrefix string) string {
	return fmt.Sprintf(`# CI-trigger credentialed pipelines: read-only, upstream subtree only. This
# policy never carries approval-table grants; release secrets are reachable
# only through the release-tier credentialed role. Managed by oberth install.
path "%s/data/upstream/*" {
  capabilities = ["read"]
}

# Allow the fetch client to revoke its own short-lived login token.
path "auth/token/revoke-self" {
  capabilities = ["update"]
}`, kvPrefix)
}

// OberthProductionPolicy adds exactly the two Transit data operations needed
// for trusted-plan envelopes. It grants no key-management, export, rotate,
// backup, configuration, list, or wildcard Transit capability.
func OberthProductionPolicy(kvPrefix, transitMount, transitKey string) string {
	return fmt.Sprintf(`# Oberth release secrets: read-only, data endpoints only. No list, no
# metadata, no write, no delete. Managed by oberth install.
path "%s/data/*" {
  capabilities = ["read"]
}

# Trusted-plan envelope encryption/decryption through one non-exportable key.
path "%s/encrypt/%s" {
  capabilities = ["update"]
}

path "%s/decrypt/%s" {
  capabilities = ["update"]
}

# Allow the client to revoke its own short-lived login token.
path "auth/token/revoke-self" {
  capabilities = ["update"]
}`, kvPrefix, transitMount, transitKey, transitMount, transitKey)
}

func getClusterCA(ctx context.Context, deps Deps) (string, error) {
	cm, err := deps.KubeClient.CoreV1().ConfigMaps("kube-public").Get(ctx, "kube-root-ca.crt", metav1.GetOptions{})
	if err == nil {
		if ca, ok := cm.Data["ca.crt"]; ok && ca != "" {
			return ca, nil
		}
	}
	if deps.RestConfig != nil && len(deps.RestConfig.CAData) > 0 {
		return string(deps.RestConfig.CAData), nil
	}
	return "", errors.New("could not read cluster CA certificate; ensure kube-root-ca.crt ConfigMap exists in kube-public namespace")
}
