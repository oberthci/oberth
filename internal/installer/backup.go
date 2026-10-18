package installer

// `oberth backup`: one encrypted archive of what a fresh install cannot
// recreate — a verified snapshot of the server database with the protocol
// identifiers needed to open it, and the secrets pipelines use: the
// operator's OpenBao KV secrets, ACL policies and Kubernetes auth roles, and
// the secret-access table — so that `oberth restore` can bring them back on
// a fresh install (issues 038, 048 and 068).
//
// Like `oberth install`, it runs on an operator workstation with kubectl,
// helm, openssl and BAO_TOKEN; every in-cluster operation is a kubectl exec or
// a Kubernetes API call, and nothing in the cluster is changed.

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/oberthci/oberth/internal/auditanchor"
	"github.com/oberthci/oberth/internal/store"
)

// BackupConfig selects the source installation and the encrypted output.
type BackupConfig struct {
	KubeconfigPath   string
	ContextName      string
	Namespace        string
	OpenBaoNamespace string
	ArgoNamespace    string
	// Output is the archive to create; it must not exist.
	Output string
	// Recipient is the PEM certificate (RSA >= 3072 bits) the archive is
	// encrypted to. Only the holder of its private key can restore.
	Recipient string
	// Scope is BackupScopeFull (default) or BackupScopeHistory: the
	// database only, with no OpenBao root token needed.
	Scope       string
	DryRun      bool
	ToolVersion string
}

// backupDeps extends Deps with what backup and restore need beyond install.
type backupDeps struct {
	Deps
	// runStdout runs a host command and returns its stdout alone (binary
	// database streams must not be mixed with kubectl's stderr warnings).
	runStdout CommandRunner
	cipher    archiveCipher
	openssl   string
	now       func() time.Time
	rootToken string
	// envToken is BAO_TOKEN as read (and cleared) at startup, so no child
	// process inherits it even when the operation never needs it.
	envToken  string
	apiServer string
}

func (cfg *BackupConfig) applyDefaults() {
	if cfg.Namespace == "" {
		cfg.Namespace = DefaultNamespace
	}
	if cfg.OpenBaoNamespace == "" {
		cfg.OpenBaoNamespace = DefaultOpenBaoNamespace
	}
	if cfg.ArgoNamespace == "" {
		cfg.ArgoNamespace = DefaultArgoNamespace
	}
	if cfg.Scope == "" {
		cfg.Scope = BackupScopeFull
	}
}

// ExecuteBackup resolves the cluster and host tools and runs the backup.
func ExecuteBackup(ctx context.Context, cfg BackupConfig, host InstallDeps) error {
	cfg.applyDefaults()
	if strings.TrimSpace(cfg.Output) == "" {
		return errors.New("--output is required: the encrypted archive to create")
	}
	if strings.TrimSpace(cfg.Recipient) == "" {
		return errors.New("--recipient is required: the PEM certificate the archive is encrypted to (see docs/backup-restore.md)")
	}
	if _, err := os.Lstat(cfg.Output); err == nil {
		return fmt.Errorf("output %s already exists; refusing to overwrite it", cfg.Output)
	}
	if info, err := os.Stat(filepath.Dir(cfg.Output)); err != nil || !info.IsDir() {
		return fmt.Errorf("output directory %s does not exist", filepath.Dir(cfg.Output))
	}
	if cfg.Scope != BackupScopeFull && cfg.Scope != BackupScopeHistory {
		return fmt.Errorf("--scope must be %s or %s", BackupScopeFull, BackupScopeHistory)
	}
	if err := validateBackupRecipient(cfg.Recipient); err != nil {
		return err
	}
	deps, err := prepareOperatorDeps(host, cfg.KubeconfigPath, cfg.ContextName, !cfg.DryRun)
	if err != nil {
		return err
	}
	deps.cipher = opensslCMS{binary: deps.openssl, recipient: cfg.Recipient}
	// Only a full backup reads OpenBao. At a terminal, Enter alone at the
	// token prompt is the way to a history-only backup.
	if cfg.Scope == BackupScopeFull && !cfg.DryRun {
		prompt := "OpenBao root token (not echoed; press Enter alone for a history-only backup without secrets): "
		if deps.rootToken, err = deps.takeRootToken(prompt); err != nil {
			return err
		}
		switch {
		case deps.rootToken != "":
		case deps.ReadPassword != nil:
			cfg.Scope = BackupScopeHistory
			_, _ = fmt.Fprintln(deps.Output, "No token given: backing up history only (runs, issues, promotions, uplinks), without secrets.")
		default:
			return fmt.Errorf("%w\n(or back up history only, without secrets: oberth backup --history-only)", missingRootTokenError("backup"))
		}
	}
	return runBackup(ctx, cfg, deps)
}

// prepareOperatorDeps mirrors Execute's host wiring: kubeconfig and context
// selection for client-go, kubectl and helm alike, and the echo-off token
// prompt. The root token itself is taken by the caller (takeRootToken) only
// when the operation needs OpenBao. A dry run (live=false) needs neither helm
// nor a token prompt.
func prepareOperatorDeps(host InstallDeps, kubeconfigPath, contextName string, live bool) (backupDeps, error) {
	host = host.withDefaults()
	if live {
		if err := CheckHostTools(host.GOOS, host.LookPath); err != nil {
			return backupDeps{}, err
		}
	}
	openssl, err := host.LookPath("openssl")
	if err != nil {
		return backupDeps{}, errors.New("openssl not found in PATH; backup archives use OpenSSL 3 `cms` (AES-256-GCM), install it and rerun")
	}

	runCommand := host.RunCommand
	runStdout := CommandRunner(defaultRunCommandStdout)
	if kubeconfigPath != "" {
		host.LoadKubeConfig = func(name string) (kubernetes.Interface, *rest.Config, string, error) {
			rules := clientcmd.NewDefaultClientConfigLoadingRules()
			rules.ExplicitPath = kubeconfigPath
			return loadKubeConfigWithRules(rules, name, "", host.Output)
		}
		runCommand = withKubeconfigArg(runCommand, kubeconfigPath)
		runStdout = withKubeconfigArg(runStdout, kubeconfigPath)
	}
	kubeClient, restConfig, selectedContext, err := host.LoadKubeConfig(contextName)
	if err != nil {
		if contextName != "" {
			return backupDeps{}, fmt.Errorf("load kubeconfig context %q: %w", contextName, err)
		}
		return backupDeps{}, fmt.Errorf("load kubeconfig: %w", err)
	}
	helmKubeArgs, _, _ := ResolveHelmKubeArgs(contextName)
	if kubeconfigPath != "" {
		helmKubeArgs = []string{"--kubeconfig", kubeconfigPath, "--kube-context", selectedContext}
	}

	var readPassword func() ([]byte, error)
	if live && host.IsTerminal != nil && host.IsTerminal() {
		readPassword = func() ([]byte, error) { return term.ReadPassword(int(os.Stdin.Fd())) }
	}
	deps := backupDeps{
		Deps: Deps{
			Output:         host.Output,
			Input:          host.Input,
			RunHelm:        HelmWithKubeArgs(host.RunHelm, helmKubeArgs),
			RunCommand:     runCommand,
			RunInteractive: host.RunInteractive,
			IsTerminal:     host.IsTerminal,
			LookPath:       host.LookPath,
			KubeClient:     kubeClient,
			RestConfig:     restConfig,
			ContextName:    selectedContext,
			ReadPassword:   readPassword,
		},
		runStdout: runStdout,
		openssl:   openssl,
		now:       time.Now,
	}
	if restConfig != nil {
		deps.apiServer = restConfig.Host
	}
	deps.envToken = strings.TrimSpace(os.Getenv(baoTokenEnvVar))
	_ = os.Unsetenv(baoTokenEnvVar)
	_ = os.Unsetenv("VAULT_TOKEN")
	return deps, nil
}

// takeRootToken returns BAO_TOKEN as read at startup or, on a terminal,
// prompts for it with echo disabled. The token reaches OpenBao only as the
// first stdin line of each kubectl exec.
func (deps backupDeps) takeRootToken(prompt string) (string, error) {
	if deps.envToken != "" || deps.ReadPassword == nil {
		return deps.envToken, nil
	}
	_, _ = fmt.Fprint(deps.Output, prompt)
	raw, err := deps.ReadPassword()
	_, _ = fmt.Fprintln(deps.Output)
	if err != nil {
		return "", fmt.Errorf("read root token: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	clear(raw)
	return token, nil
}

func missingRootTokenError(command string) error {
	return fmt.Errorf("%s needs the OpenBao root token; supply it without exposing it in shell history:\n\n"+
		"    read -rs BAO_TOKEN && export BAO_TOKEN\n\nthen rerun oberth %s", command, command)
}

// withKubeconfigArg prepends --kubeconfig to every kubectl invocation.
func withKubeconfigArg(run CommandRunner, path string) CommandRunner {
	return func(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
		if name == "kubectl" {
			args = append([]string{"--kubeconfig", path}, args...)
		}
		return run(ctx, input, name, args...)
	}
}

// defaultRunCommandStdout runs a host command and returns stdout alone; on
// failure the error carries stderr for diagnosis.
func defaultRunCommandStdout(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
	// #nosec G204 -- callers select a fixed executable and pass structured
	// arguments; no shell interpolation occurs on the host.
	command := exec.CommandContext(ctx, name, args...)
	if len(input) > 0 {
		command.Stdin = bytes.NewReader(input)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("%s: %w%s", name, err, commandOutputSuffix(stderr.Bytes()))
	}
	return stdout.Bytes(), nil
}

// displayLocalPath shows a path relative to the working directory when it
// is inside it, else as displayPath does.
func displayLocalPath(path string) string {
	if wd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(wd, path); err == nil && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
			return rel
		}
	}
	return displayPath(path)
}

// --- Server discovery shared by backup and restore ---

// serverDeployment is what backup and restore need from deploy/oberth.
type serverDeployment struct {
	Image         string
	Replicas      int32
	Selector      string
	ClaimName     string
	Compatibility backupCompatibility
	PodSpec       corev1.PodSpec
}

func inspectServerDeployment(ctx context.Context, deps Deps, namespace string) (serverDeployment, error) {
	deployment, err := deps.KubeClient.AppsV1().Deployments(namespace).Get(ctx, "oberth", metav1.GetOptions{})
	if err != nil {
		return serverDeployment{}, fmt.Errorf("get deployment %s/oberth: %w", namespace, err)
	}
	info := serverDeployment{Replicas: 1, PodSpec: deployment.Spec.Template.Spec}
	if deployment.Spec.Replicas != nil {
		info.Replicas = *deployment.Spec.Replicas
	}
	if deployment.Spec.Selector == nil || len(deployment.Spec.Selector.MatchLabels) == 0 {
		return serverDeployment{}, fmt.Errorf("deployment %s/oberth has no label selector", namespace)
	}
	info.Selector = labels.SelectorFromSet(deployment.Spec.Selector.MatchLabels).String()
	for _, volume := range deployment.Spec.Template.Spec.Volumes {
		if volume.Name == "data" && volume.PersistentVolumeClaim != nil {
			info.ClaimName = volume.PersistentVolumeClaim.ClaimName
		}
	}
	defaults := store.DefaultProtocolConfig()
	info.Compatibility = backupCompatibility{
		SchemaIdentity: defaults.SchemaIdentity,
		AuditDomain:    defaults.AuditDomain,
		WitnessKeyInfo: auditanchor.DefaultWitnessKeyInfo,
	}
	found := false
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name != serverContainerName {
			continue
		}
		found = true
		info.Image = container.Image
		for _, env := range container.Env {
			switch env.Name {
			case "OBERTH_SCHEMA_IDENTITY":
				info.Compatibility.SchemaIdentity = env.Value
			case "OBERTH_AUDIT_DOMAIN":
				info.Compatibility.AuditDomain = env.Value
			case "OBERTH_WITNESS_KEY_INFO":
				info.Compatibility.WitnessKeyInfo = env.Value
			}
		}
	}
	if !found {
		return serverDeployment{}, fmt.Errorf("deployment %s/oberth has no %q container", namespace, serverContainerName)
	}
	if info.ClaimName == "" {
		return serverDeployment{}, fmt.Errorf("deployment %s/oberth mounts no PersistentVolumeClaim as its data volume", namespace)
	}
	return info, nil
}

// runningServerPod returns a running, non-terminating server pod, or "".
func runningServerPod(ctx context.Context, deps Deps, namespace, selector string) (string, error) {
	pods, err := deps.KubeClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return "", fmt.Errorf("list server pods: %w", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp == nil && isPodRunning(pod) {
			return pod.Name, nil
		}
	}
	return "", nil
}

// connectOpenBao finds the verified OpenBao pod and requires it unsealed.
func connectOpenBao(ctx context.Context, deps Deps, namespace string) (openBaoExec, error) {
	pod, err := findOpenBaoPod(ctx, deps, namespace)
	if err != nil {
		return openBaoExec{}, err
	}
	client := newOpenBaoExec(deps, namespace, pod)
	status, err := client.status(ctx)
	if err != nil {
		return openBaoExec{}, err
	}
	if !status.Initialized || status.Sealed {
		return openBaoExec{}, fmt.Errorf("OpenBao in namespace %s is not initialized and unsealed; unseal it first", namespace)
	}
	return client, nil
}

// secretAccessExport is the oberth-secret-access ConfigMap's data. The
// ConfigMap, not the database, is the source of truth the server reconciles
// grants from at startup, so it must be restored before the server starts.
type secretAccessExport struct {
	Present bool              `json:"present"`
	Data    map[string]string `json:"data"`
}

func readSecretAccess(ctx context.Context, deps Deps, namespace string) (secretAccessExport, error) {
	cm, err := deps.KubeClient.CoreV1().ConfigMaps(namespace).Get(ctx, secretAccessConfigMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return secretAccessExport{Data: map[string]string{}}, nil
	}
	if err != nil {
		return secretAccessExport{}, fmt.Errorf("get ConfigMap %s/%s: %w", namespace, secretAccessConfigMapName, err)
	}
	data := cm.Data
	if data == nil {
		data = map[string]string{}
	}
	return secretAccessExport{Present: true, Data: data}, nil
}

// --- Backup flow ---

func runBackup(ctx context.Context, cfg BackupConfig, deps backupDeps) (resultErr error) {
	w := deps.Output
	full := cfg.Scope != BackupScopeHistory
	if full {
		_, _ = fmt.Fprintf(w, "Backing up Oberth from context %s (namespace %s, OpenBao %s): history and secrets.\n", deps.ContextName, cfg.Namespace, cfg.OpenBaoNamespace)
	} else {
		_, _ = fmt.Fprintf(w, "Backing up Oberth's history from context %s (namespace %s): runs, issues, promotions and uplinks; no secrets.\n", deps.ContextName, cfg.Namespace)
	}

	server, err := inspectServerDeployment(ctx, deps.Deps, cfg.Namespace)
	if err != nil {
		return err
	}
	pod, err := runningServerPod(ctx, deps.Deps, cfg.Namespace, server.Selector)
	if err != nil {
		return err
	}
	if pod == "" {
		return fmt.Errorf("no running server pod in namespace %s; the database snapshot is taken through the running server", cfg.Namespace)
	}
	release, found := findHelmRelease(ctx, deps.Deps, "oberth", cfg.Namespace)
	if !found {
		return fmt.Errorf("no oberth Helm release in namespace %s", cfg.Namespace)
	}
	protocol := store.ProtocolConfig{SchemaIdentity: server.Compatibility.SchemaIdentity, AuditDomain: server.Compatibility.AuditDomain}
	if err := protocol.Validate(); err != nil {
		return fmt.Errorf("deployment compatibility values: %w", err)
	}

	if cfg.DryRun {
		_, _ = fmt.Fprintln(w, "Plan (--dry-run):")
		if full {
			_, _ = fmt.Fprintf(w, "  OpenBao   every KV v2 secret under %s/, every ACL policy except root and default,\n", defaultKVPrefix)
			_, _ = fmt.Fprintf(w, "            every auth/%s role (all fields); auth config, Transit and PKI metadata for reference\n", defaultAuthMount)
		} else {
			_, _ = fmt.Fprintln(w, "  OpenBao   not read (--scope history)")
		}
		_, _ = fmt.Fprintf(w, "  Database  verified snapshot of %s through pod %s\n", serverDatabasePath, pod)
		_, _ = fmt.Fprintf(w, "            schema identity %s, audit domain %s, witness key info %s\n",
			server.Compatibility.SchemaIdentity, server.Compatibility.AuditDomain, server.Compatibility.WitnessKeyInfo)
		if full {
			_, _ = fmt.Fprintf(w, "  Grants    ConfigMap %s/%s\n", cfg.Namespace, secretAccessConfigMapName)
		}
		_, _ = fmt.Fprintf(w, "  Output    %s, encrypted to %s\n", cfg.Output, cfg.Recipient)
		_, _ = fmt.Fprintln(w, "Nothing was read from OpenBao or written to disk (--dry-run).")
		return nil
	}

	schema, err := podSchemaVersion(ctx, deps.RunCommand, deps.ContextName, cfg.Namespace, pod, serverContainerName)
	if err != nil {
		return err
	}
	if schema > store.LatestMigrationVersion() {
		return fmt.Errorf("the server writes database schema %d but this oberth CLI understands only up to %d; upgrade the CLI first",
			schema, store.LatestMigrationVersion())
	}

	work, err := os.MkdirTemp("", "oberth-backup-")
	if err != nil {
		return fmt.Errorf("create private working directory: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(work)) }()

	var secretCount, policyCount, roleCount int
	var reference openBaoReference
	if full {
		bao, err := connectOpenBao(ctx, deps.Deps, cfg.OpenBaoNamespace)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(w, "Reading OpenBao...")
		kv, err := exportKV(ctx, bao, deps.rootToken, defaultKVPrefix)
		if err != nil {
			return err
		}
		kv.Secrets = operatorSecrets(kv.Secrets)
		policies, err := exportPolicies(ctx, bao, deps.rootToken)
		if err != nil {
			return err
		}
		policies = operatorPolicies(policies)
		roles, err := exportRoles(ctx, bao, deps.rootToken, defaultAuthMount)
		if err != nil {
			return err
		}
		roles = operatorRoles(roles)
		reference = exportOpenBaoReference(ctx, bao, deps.rootToken)
		for _, member := range []struct {
			name  string
			value any
		}{
			{backupMemberKV, kv},
			{backupMemberPolicies, policies},
			{backupMemberRoles, roles},
			{backupMemberReference, reference},
		} {
			if err := writeBackupJSON(work, member.name, member.value); err != nil {
				return err
			}
		}
		secretCount, policyCount, roleCount = len(kv.Secrets), len(policies), len(roles.Roles)
		// The decoded secret values are no longer needed once written.
		kv.Secrets = nil
	}

	_, _ = fmt.Fprintln(w, "Snapshotting the server database...")
	databasePath := filepath.Join(work, filepath.FromSlash(backupMemberDatabase))
	if err := os.MkdirAll(filepath.Dir(databasePath), 0o700); err != nil {
		return err
	}
	snapshotMode, err := snapshotServerDatabase(ctx, deps, cfg.Namespace, pod, schema, protocol, work, databasePath)
	if err != nil {
		return err
	}
	report, err := verifyDatabaseArchive(ctx, databasePath, schema, protocol)
	if err != nil {
		return err
	}
	contents, err := summarizeDatabaseArchive(ctx, databasePath, work, protocol)
	if err != nil {
		return err
	}

	var grants secretAccessExport
	if full {
		if grants, err = readSecretAccess(ctx, deps.Deps, cfg.Namespace); err != nil {
			return err
		}
		if err := writeBackupJSON(work, backupMemberSecretAccess, grants); err != nil {
			return err
		}
	}

	serverVersion := ""
	if out, err := deps.RunCommand(ctx, nil, "kubectl", kubectlExecArgs(deps.ContextName, cfg.Namespace, pod, serverContainerName, false, "oberth", "version")...); err == nil {
		serverVersion = strings.TrimSpace(string(out))
	}
	manifest := backupManifest{
		Format:      backupFormat,
		CreatedAt:   deps.now().UTC().Format(time.RFC3339),
		Scope:       cfg.Scope,
		ToolVersion: cfg.ToolVersion,
		Source: backupSource{
			Context:          deps.ContextName,
			APIServer:        deps.apiServer,
			Namespace:        cfg.Namespace,
			OpenBaoNamespace: cfg.OpenBaoNamespace,
			ArgoNamespace:    cfg.ArgoNamespace,
			ServerImage:      server.Image,
			ServerVersion:    serverVersion,
			ChartVersion:     chartVersionFromRelease(release.Chart, "oberth"),
			ClusterUID:       clusterUID(ctx, deps),
		},
		Contents:      backupContentsOf(contents),
		Compatibility: server.Compatibility,
		Database: backupDatabaseSummary{
			Schema:          report.Schema,
			AuditHeadID:     report.AuditHead.ID,
			AuditHeadSHA256: hex.EncodeToString(report.AuditHead.SHA256),
			Snapshot:        snapshotMode,
		},
		OpenBao: backupOpenBaoSummary{
			KVMount:   defaultKVPrefix,
			Secrets:   secretCount,
			Policies:  policyCount,
			AuthMount: defaultAuthMount,
			Roles:     roleCount,
		},
		NotRestored: backupNotRestored,
	}
	if err := writeBackupArchive(ctx, work, &manifest, deps.cipher, cfg.Output); err != nil {
		return err
	}
	cipherSHA, err := fileSHA256(cfg.Output)
	if err != nil {
		return err
	}

	size := int64(0)
	if info, err := os.Stat(cfg.Output); err == nil {
		size = info.Size()
	}
	_, _ = fmt.Fprintf(w, "\nBacked up to %s (%.1f MiB)\n", displayLocalPath(cfg.Output), float64(size)/(1<<20))
	_, _ = fmt.Fprintf(w, "  History    %s\n", describeContents(contents))
	if full {
		_, _ = fmt.Fprintf(w, "  Secrets    %d OpenBao secrets, %d policies, %d roles, %d grants\n",
			manifest.OpenBao.Secrets, manifest.OpenBao.Policies, manifest.OpenBao.Roles, countGrants(grants))
	} else {
		_, _ = fmt.Fprintln(w, "  Secrets    none: history only (restoring it keeps the target's secrets)")
	}
	_, _ = fmt.Fprintf(w, "  Verified   database schema %d, integrity, foreign keys and the audit chain to entry %d (%s snapshot)\n",
		report.Schema, report.AuditHead.ID, snapshotMode)
	keyNote := "the private key of " + displayLocalPath(cfg.Recipient)
	if strings.HasSuffix(cfg.Recipient, backupRecipientCertName) {
		keyNote = displayLocalPath(strings.TrimSuffix(cfg.Recipient, backupRecipientCertName) + backupRecipientKeyName)
	}
	_, _ = fmt.Fprintf(w, "  Encrypted  restoring needs %s: keep a copy of it OFF this machine\n", keyNote)
	_, _ = fmt.Fprintf(w, "  SHA-256    %s\n", cipherSHA)
	if len(reference.Warnings) > 0 {
		_, _ = fmt.Fprintf(w, "  Note       %d reference-only reads failed and are recorded in the archive\n", len(reference.Warnings))
	}
	if full {
		_, _ = fmt.Fprintln(w, "\nNot in the backup: server files (git caches, step logs; repositories are cloned again), Helm values (re-apply your own),\n"+
			"and what every install makes for itself: its identities, Transit key and server CA.")
	}
	_, _ = fmt.Fprintf(w, "Look inside without a cluster: oberth restore --inspect %s\n", displayLocalPath(cfg.Output))
	_, _ = fmt.Fprintf(w, "Restore it on a fresh install: oberth restore %s\n", displayLocalPath(cfg.Output))
	return nil
}
