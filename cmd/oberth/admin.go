package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/auth"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
)

const administrativeActor = "admin@localhost"

// defaultPodNamespace returns the namespace of the pod the command runs in,
// read from the mounted ServiceAccount projection, so in-pod administrative
// commands need no --namespace flag. Outside a pod it falls back to "oberth".
func defaultPodNamespace() string {
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return "oberth"
	}
	if value := strings.TrimSpace(string(data)); value != "" {
		return value
	}
	return "oberth"
}

type upstreamDependencies struct {
	input            io.Reader
	kubernetesClient func() (kubernetes.Interface, error)
	scanHostKeys     app.SSHHostKeyScanner
	probe            app.SSHAuthProbe
	mutationGate     func(context.Context, string, string) error
	// probeInterval and probeWait override the registration-wait timings for
	// tests; zero values select the production defaults (10s / 10m).
	probeInterval time.Duration
	probeWait     time.Duration
}

// hostUpstreamDeps holds injectable dependencies for the host-side
// upstream add flow. The host never dials the pod-private mutation gate,
// database, or Kubernetes API for Secret mutations. A provided key is
// streamed over kubectl exec stdin to the in-pod provide-key handler,
// which gates, applies, and verifies.
type hostUpstreamDeps struct {
	input io.Reader
	// loadTarget returns the kubeconfig context name and API server URL for
	// target disclosure. Nil selects no disclosure.
	loadTarget func() (contextName, apiServer string, err error)
	// runInteractive runs a command with the operator's terminal attached.
	// Used to exec into the pod for the full upstream add flow. Nil selects
	// the default (os.Stdin/Stdout/Stderr).
	runInteractive func(ctx context.Context, name string, args ...string) error
	// runWithStdin runs a command piping the given reader to its stdin.
	// Used to stream a provided key into the pod's provide-key handler.
	// Nil selects the default (pipe stdin, os.Stdout/os.Stderr).
	runWithStdin func(ctx context.Context, stdin io.Reader, name string, args ...string) error
}

type uplinkDependencies struct {
	mutationGate func(context.Context, string, string) error
}

func runUpstream(ctx context.Context, arguments []string, output io.Writer) error {
	// Validate arguments before any kubeconfig work. Subcommands that do not
	// need a Kubernetes client (list, remove, empty/unknown) must not pay the
	// cost or fail on a missing kubeconfig.
	if len(arguments) == 0 || (arguments[0] != "add" && arguments[0] != "provide-key") {
		return runUpstreamWithDependencies(ctx, arguments, output, upstreamDependencies{
			input:        os.Stdin,
			mutationGate: requestLiveAdminMutationGate,
		})
	}
	// "upstream add" and "upstream provide-key" need a Kubernetes client.
	// Try in-cluster config first (running inside the pod). When that fails,
	// fall back to the host kubeconfig for "add" only; provide-key is pod-only.
	inCluster, inClusterErr := rest.InClusterConfig()
	if inClusterErr == nil {
		inCluster.Timeout = 10 * time.Second
		return runUpstreamWithDependencies(ctx, arguments, output, upstreamDependencies{
			input: os.Stdin,
			kubernetesClient: func() (kubernetes.Interface, error) {
				return kubernetes.NewForConfig(inCluster)
			},
			scanHostKeys: app.ScanSSHHostKeys,
			probe:        app.ProbeSSHAuthentication,
			mutationGate: requestLiveAdminMutationGate,
		})
	}
	// Host mode: provide-key is only available inside the Oberth pod.
	if arguments[0] == "provide-key" {
		return errors.New("upstream provide-key is only available inside the Oberth pod")
	}
	// Host mode for "add": defer kubeconfig loading to first use so that
	// --help does not fail on a host without a kubeconfig. The host never
	// dials the pod-private mutation gate, database, or Kubernetes API for
	// Secret mutations — it execs into the pod for all gated operations.
	return runUpstreamAddFromHost(ctx, arguments[1:], output, hostUpstreamDeps{
		input: os.Stdin,
		loadTarget: func() (string, string, error) {
			_, restConfig, ctxName, err := installer.LoadKubeConfigForContext("", output)
			if err != nil {
				return "", "", err
			}
			return ctxName, restConfig.Host, nil
		},
	})
}

func runUpstreamWithDependencies(ctx context.Context, arguments []string, output io.Writer, dependencies upstreamDependencies) error {
	if len(arguments) == 0 {
		return fmt.Errorf("%w: upstream add|list|remove", errUsage)
	}
	switch arguments[0] {
	case "add":
	case "provide-key":
		return runUpstreamProvideKey(ctx, arguments[1:], dependencies.input, output, dependencies)
	case "list":
		return runUpstreamList(ctx, arguments[1:], output)
	case "remove":
		return runUpstreamRemove(ctx, arguments[1:], dependencies.input, output, dependencies.mutationGate)
	default:
		return fmt.Errorf("%w: upstream add [flags] <name> <base-url> | upstream list | upstream remove <name>", errUsage)
	}
	flags := flag.NewFlagSet("upstream add", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	databasePath := flags.String("database", "/data/oberth.sqlite", "SQLite database path (in-pod; requires the live admin daemon)")
	upstreamKey := flags.String("upstream-key", "/etc/oberth/upstream-key/id_ed25519", "upstream SSH private key")
	knownHosts := flags.String("known-hosts", "/etc/oberth/known-hosts/known_hosts", "upstream known_hosts")
	namespace := flags.String("namespace", defaultPodNamespace(), "Kubernetes namespace")
	upstreamKeySecret := flags.String("upstream-key-secret", "oberth-upstream-key", "Secret holding the upstream SSH private keys")
	knownHostsSecret := flags.String("known-hosts-secret", "oberth-known-hosts", "upstream known_hosts Secret")
	noWait := flags.Bool("no-wait", false, "print the deploy key and exit immediately instead of waiting for its registration")
	autoYes := flags.Bool("yes", false, "auto-accept key generation and host-key prompts")
	dedicatedKey := flags.Bool("dedicated-key", false, "provision a dedicated SSH identity for this upstream instead of the shared key (stored as an additional data key of the upstream-key Secret)")
	expectedHostFingerprint := flags.String("expected-host-fingerprint", "", "expected SSH host-key fingerprint (SHA256:...) for unattended bootstrap of unknown forges")
	if err := flags.Parse(arguments[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 2 {
		return fmt.Errorf("%w: upstream add requires name and base URL", errUsage)
	}
	name, baseURL := flags.Arg(0), strings.TrimSuffix(flags.Arg(1), "/")
	if err := app.ValidateUpstreamName(name); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	kind, err := app.UpstreamKind(baseURL)
	if err != nil {
		return err
	}
	// With --dedicated-key, this upstream gets its own identity stored as an
	// additional data key of the one chart-owned upstream-key Secret. The
	// existing whole-Secret volume mount projects it as a file beside the
	// shared key, so the server reads it from tmpfs without any Kubernetes
	// API access and RBAC stays scoped to the two named Secrets. Without the
	// flag — and for every pre-existing upstream row — the shared key keeps
	// today's semantics exactly, including the no-API projected fast path.
	if *dedicatedKey && kind != "ssh" {
		return fmt.Errorf("%w: --dedicated-key applies only to SSH upstreams", errUsage)
	}
	keyName := ""
	if *dedicatedKey && kind == "ssh" {
		keyName = app.UpstreamKeyDataKey(filepath.Base(*upstreamKey), name)
	}
	if kind == "ssh" {
		privateKeyPath, privateDataKey, keyFieldManager := *upstreamKey, filepath.Base(*upstreamKey), ""
		if keyName != "" {
			// The dedicated identity lives beside the shared one: the fast
			// path re-uses an already projected dedicated key, and a missing
			// file selects the generation flow for exactly this data key
			// under this upstream's own server-side-apply field manager.
			privateKeyPath = filepath.Join(filepath.Dir(*upstreamKey), keyName)
			privateDataKey = keyName
			keyFieldManager = app.UpstreamKeyFieldManager(name)
		}
		secretGate := func(ctx context.Context, operation string) error {
			if dependencies.mutationGate == nil {
				return errors.New("admin audit mutation gate is unavailable")
			}
			return dependencies.mutationGate(ctx, operation, *databasePath)
		}
		if _, err := (app.UpstreamSSHBootstrap{
			Input: dependencies.input, Output: output,
			KubernetesClient:        dependencies.kubernetesClient,
			ScanHostKeys:            dependencies.scanHostKeys,
			Probe:                   dependencies.probe,
			Namespace:               *namespace,
			PrivateKeySecret:        *upstreamKeySecret,
			KnownHostsSecret:        *knownHostsSecret,
			PrivateKeyPath:          privateKeyPath,
			KnownHostsPath:          *knownHosts,
			PrivateKeyDataKey:       privateDataKey,
			PublicKeyDataKey:        privateDataKey + ".pub",
			KnownHostsDataKey:       filepath.Base(*knownHosts),
			KeyFieldManager:         keyFieldManager,
			MutationGate:            secretGate,
			ProbeInterval:           dependencies.probeInterval,
			ProbeWait:               dependencies.probeWait,
			NoWait:                  *noWait,
			AutoYes:                 *autoYes,
			ExpectedHostFingerprint: *expectedHostFingerprint,
		}).Ensure(ctx, baseURL); err != nil {
			if errors.Is(err, app.ErrUpstreamBootstrapIncomplete) {
				// The flow completed normally: the key was generated, persisted,
				// and printed. Registering it upstream is the user's next step,
				// not a failure, so this path exits zero.
				rerun := upstreamAddRerunCommand(name, baseURL, *dedicatedKey, *namespace, *upstreamKeySecret, *knownHostsSecret, *expectedHostFingerprint)
				_, printErr := fmt.Fprintf(output,
					"→ Register this key with the upstream, then rerun:\n\n    %s\n", rerun)
				return printErr
			}
			return fmt.Errorf("upstream SSH identity is not ready: %w", err)
		}
	}
	if dependencies.mutationGate == nil {
		return errors.New("admin audit mutation gate is unavailable")
	}
	if err := dependencies.mutationGate(ctx, "upstream.database.open", *databasePath); err != nil {
		return err
	}
	database, err := store.OpenAdminClient(ctx, *databasePath, store.Options{})
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()
	if err := dependencies.mutationGate(ctx, "upstream.register", *databasePath); err != nil {
		return err
	}
	if _, err := database.RegisterUpstream(ctx, administrativeActor, model.UpstreamSpec{
		Name: name, Kind: kind, BaseURL: baseURL, KeyName: keyName,
	}); err != nil {
		return err
	}
	if keyName != "" {
		// The server assembles its SSH configuration once at startup, and the
		// kubelet needs a short interval to project a freshly applied data
		// key onto the Secret volume. Without a restart, Git operations for
		// this upstream keep using the shared fallback key.
		if _, err := fmt.Fprintf(output,
			"Per-upstream key stored as data key %s. Restart the server (kubectl rollout restart deploy/oberth) to activate it for Git operations.\n",
			keyName); err != nil {
			return err
		}
	}
	return nil
}

// maximumDeployKeyBytes is the file size cap for a provided deploy key.
const maximumDeployKeyBytes = 1 << 20

// runUpstreamAddFromHost handles `upstream add` from the host. It parses the
// same flags as the in-pod path, optionally runs a [G]enerate / [P]rovide
// key selection prompt, stores a provided key in the Secret via the
// Kubernetes API (the same pattern as installer/onboard.go
// applyProvidedDeployKey), then execs into the pod for the full upstream add
// flow (SSH bootstrap, mutation gate, database registration).
func runUpstreamAddFromHost(ctx context.Context, arguments []string, output io.Writer, deps hostUpstreamDeps) error {
	flags := flag.NewFlagSet("upstream add", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	namespace := flags.String("namespace", defaultPodNamespace(), "Kubernetes namespace")
	upstreamKeySecret := flags.String("upstream-key-secret", defaultUpstreamKeySecret, "Secret holding the upstream SSH private keys")
	knownHostsSecret := flags.String("known-hosts-secret", defaultUpstreamKnownHostsSecret, "upstream known_hosts Secret")
	autoYes := flags.Bool("yes", false, "auto-accept key generation and host-key prompts")
	dedicatedKey := flags.Bool("dedicated-key", false, "provision a dedicated SSH identity for this upstream")
	expectedHostFingerprint := flags.String("expected-host-fingerprint", "", "expected SSH host-key fingerprint (SHA256:...)")
	noWait := flags.Bool("no-wait", false, "print the deploy key and exit immediately instead of waiting for its registration")
	// These flags are accepted but only consumed by the in-pod command:
	_ = flags.String("database", "", "")
	_ = flags.String("upstream-key", "", "")
	_ = flags.String("known-hosts", "", "")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 2 {
		return fmt.Errorf("%w: upstream add requires name and base URL", errUsage)
	}
	name, baseURL := flags.Arg(0), strings.TrimSuffix(flags.Arg(1), "/")
	if err := app.ValidateUpstreamName(name); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	kind, err := app.UpstreamKind(baseURL)
	if err != nil {
		return err
	}

	// Target disclosure: before the first mutating action, print the
	// kubeconfig context and API server, matching oberth upgrade's format.
	if deps.loadTarget != nil {
		contextName, apiServer, targetErr := deps.loadTarget()
		if targetErr != nil {
			return fmt.Errorf("load kubeconfig: %w", targetErr)
		}
		if _, err := fmt.Fprintf(output, "Target: %s (%s)\n", contextName, apiServer); err != nil {
			return err
		}
	}

	// On the host, offer [G]enerate / [P]rovide for SSH upstreams.
	// --yes auto-selects Generate (the pod handles generation).
	provided := false
	if kind == "ssh" && !*autoYes {
		var chosen bool
		for !chosen {
			if _, err := fmt.Fprint(output, "Generate a new deploy key or provide an existing one? [G]enerate / [P]rovide: "); err != nil {
				return err
			}
			answer, err := readLineFromReader(deps.input)
			if err != nil {
				return fmt.Errorf("read deploy-key choice: %w", err)
			}
			switch strings.ToLower(strings.TrimSpace(answer)) {
			case "", "g", "generate":
				chosen = true
			case "p", "provide":
				if err := hostProvideDeployKey(ctx, deps, *namespace, *upstreamKeySecret, name, *dedicatedKey, output); err != nil {
					return err
				}
				provided = true
				chosen = true
			default:
				if _, err := fmt.Fprintln(output, "Please answer G or P."); err != nil {
					return err
				}
			}
		}
	}

	// Build the kubectl exec args for the in-pod command. The pod handles
	// SSH bootstrap (finding the persisted key or generating), host-key
	// trust, authentication probe, mutation gate, and database registration.
	execArgs := []string{"exec"}
	if !*autoYes && !provided {
		// Interactive: the pod prompts for generation confirmation and
		// host-key trust; wire stdin.
		execArgs = append(execArgs, "-it")
	} else {
		execArgs = append(execArgs, "-i")
	}
	execArgs = append(execArgs, "-c", "oberth", "-n", *namespace, "deploy/oberth", "--", "oberth", "upstream", "add")
	if *autoYes || provided {
		// --yes: auto-confirm generation or skip it (key already persisted).
		execArgs = append(execArgs, "--yes")
	}
	if *dedicatedKey {
		execArgs = append(execArgs, "--dedicated-key")
	}
	if *expectedHostFingerprint != "" {
		execArgs = append(execArgs, "--expected-host-fingerprint", *expectedHostFingerprint)
	}
	if *noWait {
		execArgs = append(execArgs, "--no-wait")
	}
	if *upstreamKeySecret != defaultUpstreamKeySecret {
		execArgs = append(execArgs, "--upstream-key-secret", *upstreamKeySecret)
	}
	if *knownHostsSecret != defaultUpstreamKnownHostsSecret {
		execArgs = append(execArgs, "--known-hosts-secret", *knownHostsSecret)
	}
	execArgs = append(execArgs, name, baseURL)

	runner := deps.runInteractive
	if runner == nil {
		runner = defaultRunInteractive
	}
	return runner(ctx, "kubectl", execArgs...)
}

// defaultRunInteractive runs a command with the operator's own terminal
// attached so the pod-side interactive flow works.
func defaultRunInteractive(ctx context.Context, name string, args ...string) error {
	// #nosec G204 -- callers select a fixed executable and pass structured
	// arguments; no shell interpolation occurs.
	command := exec.CommandContext(ctx, name, args...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return command.Run()
}

// hostProvideDeployKey reads an operator-supplied SSH private key from the
// host, validates it locally, and streams it into the pod via kubectl exec.
// The in-pod provide-key handler re-validates, checks overwrite semantics,
// gates via the live daemon's audit gate, and applies via SSA patch. The
// host never writes to the Kubernetes API directly. Passphrase-protected
// and group/other-accessible key files are rejected.
func hostProvideDeployKey(ctx context.Context, deps hostUpstreamDeps, namespace, secretName, upstreamName string, dedicatedKey bool, output io.Writer) error {
	if _, err := fmt.Fprint(output, "Path to the existing SSH PRIVATE key [~/.ssh/id_ed25519]: "); err != nil {
		return err
	}
	path, err := readLineFromReader(deps.input)
	if err != nil {
		return fmt.Errorf("read deploy-key path: %w", err)
	}
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		trimmed = "~/.ssh/id_ed25519"
	}
	expanded, err := expandHome(trimmed)
	if err != nil {
		return err
	}
	info, statErr := os.Stat(expanded)
	if statErr != nil {
		return fmt.Errorf("read deploy key: %w", statErr)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("read deploy key: %s is not a regular file", expanded)
	}
	if info.Size() > maximumDeployKeyBytes {
		return fmt.Errorf("read deploy key: %s exceeds the 1 MiB size limit", expanded)
	}
	// Permission parity with in-pod validateOperatorFile: refuse key files
	// that are writable by group or accessible by other.
	if info.Mode().Perm()&0o007 != 0 || info.Mode().Perm()&0o030 != 0 {
		return fmt.Errorf("read deploy key: %s has unsafe permissions %04o; private keys must not be writable by group or accessible by other", expanded, info.Mode().Perm())
	}
	// #nosec G304 -- the operator explicitly names their own key file.
	privateKey, err := os.ReadFile(expanded)
	if err != nil {
		return fmt.Errorf("read deploy key: %w", err)
	}
	defer func() {
		for i := range privateKey {
			privateKey[i] = 0
		}
	}()
	// Local validation: the same checks the in-pod handler applies, catching
	// obvious errors before a round-trip through kubectl exec.
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		var passphraseErr *ssh.PassphraseMissingError
		if errors.As(err, &passphraseErr) {
			return errors.New("the key is passphrase-protected; the pod needs a non-interactive key — generate a dedicated deploy key instead")
		}
		return fmt.Errorf("parse SSH private key: %w", err)
	}
	_ = signer // validated; the pod re-validates after receiving it

	// Stream the key into the pod via kubectl exec -i. The in-pod provide-key
	// handler validates, checks overwrite, gates, and applies via SSA patch.
	execArgs := []string{"exec", "-i", "-c", "oberth", "-n", namespace, "deploy/oberth", "--",
		"oberth", "upstream", "provide-key",
		"--namespace", namespace,
		"--upstream-key-secret", secretName,
	}
	if dedicatedKey {
		execArgs = append(execArgs, "--dedicated-key")
	}
	execArgs = append(execArgs, upstreamName)

	runner := deps.runWithStdin
	if runner == nil {
		runner = defaultRunWithStdin
	}
	return runner(ctx, bytes.NewReader(privateKey), "kubectl", execArgs...)
}

// defaultRunWithStdin runs a command with the given reader piped to its
// stdin. Used to stream a provided key into the pod.
func defaultRunWithStdin(ctx context.Context, stdin io.Reader, name string, args ...string) error {
	// #nosec G204 -- callers select a fixed executable and pass structured
	// arguments; no shell interpolation occurs.
	command := exec.CommandContext(ctx, name, args...)
	command.Stdin = stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return command.Run()
}

// runUpstreamProvideKey is the in-pod handler for a host-streamed deploy key.
// The host validates the key locally and streams it over kubectl exec stdin;
// this handler re-validates, checks overwrite semantics, gates via the live
// daemon's audit gate, applies via SSA patch (the same pattern as
// UpstreamSSHBootstrap.applySecret), and verifies the readback. It is a
// hidden subcommand — not shown in usage — reachable only through the exec.
func runUpstreamProvideKey(ctx context.Context, arguments []string, input io.Reader, output io.Writer, dependencies upstreamDependencies) error {
	flags := flag.NewFlagSet("upstream provide-key", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	databasePath := flags.String("database", "/data/oberth.sqlite", "SQLite database path")
	namespace := flags.String("namespace", defaultPodNamespace(), "Kubernetes namespace")
	upstreamKeySecret := flags.String("upstream-key-secret", defaultUpstreamKeySecret, "Secret holding the upstream SSH private keys")
	dedicatedKey := flags.Bool("dedicated-key", false, "store as a per-upstream dedicated key")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("%w: upstream provide-key requires the upstream name", errUsage)
	}
	name := flags.Arg(0)
	if err := app.ValidateUpstreamName(name); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}

	// Determine data keys and field manager.
	dataKey := "id_ed25519"
	fieldManager := "oberth-upstream-bootstrap"
	if *dedicatedKey {
		dataKey = app.UpstreamKeyDataKey(dataKey, name)
		fieldManager = app.UpstreamKeyFieldManager(name)
	}
	publicDataKey := dataKey + ".pub"

	// Read bounded stdin — the host streams the private key.
	privateKey, err := io.ReadAll(io.LimitReader(input, maximumDeployKeyBytes+1))
	if err != nil {
		return fmt.Errorf("read deploy key from stdin: %w", err)
	}
	defer func() {
		for i := range privateKey {
			privateKey[i] = 0
		}
	}()
	if len(privateKey) == 0 {
		return errors.New("no deploy key data received on stdin")
	}
	if len(privateKey) > maximumDeployKeyBytes {
		return errors.New("deploy key on stdin exceeds the 1 MiB size limit")
	}

	// Validate: the host already validated, but the pod re-validates as the
	// trust boundary — the exec pipe is the security perimeter.
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		var passphraseErr *ssh.PassphraseMissingError
		if errors.As(err, &passphraseErr) {
			return errors.New("the key is passphrase-protected; the pod needs a non-interactive key")
		}
		return fmt.Errorf("parse SSH private key: %w", err)
	}
	publicKey := append(bytes.TrimSpace(ssh.MarshalAuthorizedKey(signer.PublicKey())), []byte(" oberth\n")...)

	// Get Kubernetes client.
	if dependencies.kubernetesClient == nil {
		return errors.New("kubernetes client is required for upstream provide-key")
	}
	client, err := dependencies.kubernetesClient()
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}

	// Check overwrite: refuse to replace an existing key at this data key.
	existing, err := client.CoreV1().Secrets(*namespace).Get(ctx, *upstreamKeySecret, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("read Secret %s/%s: %w", *namespace, *upstreamKeySecret, err)
	}
	if err == nil && len(existing.Data[dataKey]) > 0 {
		return fmt.Errorf("the Secret %s/%s already holds a key at %s; refusing to overwrite (delete the data key first if replacement is intended)", *namespace, *upstreamKeySecret, dataKey)
	}

	// Gate: the live daemon's fail-closed audit gate must approve before
	// any Secret mutation.
	if dependencies.mutationGate == nil {
		return errors.New("admin audit mutation gate is unavailable")
	}
	if err := dependencies.mutationGate(ctx, "upstream.secret."+*upstreamKeySecret, *databasePath); err != nil {
		return err
	}

	// Apply via SSA patch — the same pattern as UpstreamSSHBootstrap.applySecret.
	data := map[string][]byte{
		dataKey:       privateKey,
		publicDataKey: publicKey,
	}
	secret := corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: *upstreamKeySecret, Namespace: *namespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
	body, err := json.Marshal(secret)
	if err != nil {
		return fmt.Errorf("encode upstream Secret apply: %w", err)
	}
	force := true
	if _, err := client.CoreV1().Secrets(*namespace).Patch(ctx, *upstreamKeySecret, types.ApplyPatchType, body, metav1.PatchOptions{
		FieldManager: fieldManager,
		Force:        &force,
	}); err != nil {
		return fmt.Errorf("apply upstream Secret %s/%s: %w", *namespace, *upstreamKeySecret, err)
	}

	// Verify readback.
	verifySecret, err := client.CoreV1().Secrets(*namespace).Get(ctx, *upstreamKeySecret, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("verify upstream Secret %s/%s: %w", *namespace, *upstreamKeySecret, err)
	}
	if !bytes.Equal(verifySecret.Data[dataKey], privateKey) {
		return fmt.Errorf("upstream Secret %s/%s did not retain data key %s", *namespace, *upstreamKeySecret, dataKey)
	}

	if _, err := fmt.Fprintf(output, "Stored the provided key in Secret %s/%s.\n", *namespace, *upstreamKeySecret); err != nil {
		return err
	}
	return nil
}

// readLineFromReader reads one line from reader without buffering ahead. This
// is the same single-byte-at-a-time approach as the installer's readLine,
// preserved so the same reader can later be piped to an interactive flow.
func readLineFromReader(reader io.Reader) (string, error) {
	if reader == nil {
		return "", errors.New("no input available")
	}
	var line []byte
	buffer := make([]byte, 1)
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			if buffer[0] == 0x03 { // ETX — Ctrl+C
				return "", installer.ErrInterrupted
			}
			if buffer[0] == '\n' {
				break
			}
			line = append(line, buffer[0])
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(line) > 0 {
					break
				}
				return "", io.EOF
			}
			return "", err
		}
	}
	return strings.TrimSuffix(string(line), "\r"), nil
}

// expandHome resolves ~ to the user's home directory. Matches the installer's
// expandHomePath.
func expandHome(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/")), nil
	}
	return path, nil
}

// Default upstream secret names. Extracted to package-level constants so
// the rerun-command builder can reference them without a hardcoded string
// literal tripping gosec G101 (which flags string variables containing
// "secret" or "key" beside what looks like a value assignment).
const (
	defaultUpstreamKeySecret        = "oberth-upstream-key" // #nosec G101 -- name, not a credential
	defaultUpstreamKnownHostsSecret = "oberth-known-hosts"  // #nosec G101 -- name, not a credential
)

func upstreamAddRerunCommand(name, baseURL string, dedicatedKey bool, namespace, keySecret, knownHostsSecret, expectedFingerprint string) string {
	var parts []string
	parts = append(parts, "oberth upstream add")
	if dedicatedKey {
		parts = append(parts, "--dedicated-key")
	}
	defaultNS := defaultPodNamespace()
	if namespace != defaultNS {
		parts = append(parts, "--namespace", shellQuote(namespace))
	}
	if keySecret != defaultUpstreamKeySecret {
		parts = append(parts, "--upstream-key-secret", shellQuote(keySecret))
	}
	if knownHostsSecret != defaultUpstreamKnownHostsSecret {
		parts = append(parts, "--known-hosts-secret", shellQuote(knownHostsSecret))
	}
	if expectedFingerprint != "" {
		parts = append(parts, "--expected-host-fingerprint", shellQuote(expectedFingerprint))
	}
	parts = append(parts, shellQuote(name), shellQuote(baseURL))
	return strings.Join(parts, " ")
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		if isShellSafe(r) {
			continue
		}
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	return s
}

func isShellSafe(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z':
		return true
	case r >= 'A' && r <= 'Z':
		return true
	case r >= '0' && r <= '9':
		return true
	}
	switch r {
	case '-', '_', '/', '.', ':', '@', '+', '=':
		return true
	}
	return false
}

// runUpstreamList prints every configured upstream with its SSH key
// fingerprint. An upstream with a dedicated key name reads the projected
// per-upstream key file beside the shared --upstream-key; others use the
// shared key. Everything comes from the Secret volume mount, so listing
// never contacts the Kubernetes API. Listing is read-only: no mutation gate
// is required and an unreadable key degrades to "-" instead of failing the
// listing. Only fingerprints of the public halves are ever printed.
func runUpstreamList(ctx context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("upstream list", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	databasePath := flags.String("database", "/data/oberth.sqlite", "SQLite database path (in-pod; requires the live admin daemon)")
	upstreamKey := flags.String("upstream-key", "/etc/oberth/upstream-key/id_ed25519", "upstream SSH private key")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: upstream list accepts no arguments", errUsage)
	}
	database, err := store.OpenAdminClient(ctx, *databasePath, store.Options{})
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()
	upstreams, err := database.ListUpstreams(ctx)
	if err != nil {
		return err
	}
	fileFingerprint := func(path string) string {
		data, readErr := os.ReadFile(path) // #nosec G304 -- operator-configured identity directory.
		if readErr != nil {
			return "-"
		}
		signer, parseErr := ssh.ParsePrivateKey(data)
		if parseErr != nil {
			return "-"
		}
		return ssh.FingerprintSHA256(signer.PublicKey())
	}
	globalFingerprint := fileFingerprint(*upstreamKey)
	keyDirectory := filepath.Dir(*upstreamKey)
	writer := tabwriter.NewWriter(output, 0, 0, 3, ' ', 0)
	if _, err := fmt.Fprintln(writer, "NAME\tKIND\tURL\tKEY NAME\tKEY FINGERPRINT"); err != nil {
		return err
	}
	for _, upstream := range upstreams {
		fingerprint := "-"
		keyName := "-"
		if upstream.Kind == "ssh" {
			switch {
			case upstream.KeyName == "":
				fingerprint = globalFingerprint
			case app.ValidUpstreamKeyName(upstream.KeyName):
				keyName = upstream.KeyName
				fingerprint = fileFingerprint(filepath.Join(keyDirectory, upstream.KeyName))
			}
		}
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", upstream.Name, upstream.Kind, upstream.BaseURL, keyName, fingerprint); err != nil {
			return err
		}
	}
	return writer.Flush()
}

// runUpstreamRemove removes one upstream and its mapped repositories after an
// explicit "yes" confirmation. Repositories holding immutable CI history fail
// the removal closed inside the store transaction.
func runUpstreamRemove(ctx context.Context, arguments []string, input io.Reader, output io.Writer, mutationGate func(context.Context, string, string) error) error {
	flags := flag.NewFlagSet("upstream remove", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	databasePath := flags.String("database", "/data/oberth.sqlite", "SQLite database path (in-pod; requires the live admin daemon)")
	assumeYes := flags.Bool("yes", false, "skip the interactive confirmation")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("%w: upstream remove [--yes] <name>", errUsage)
	}
	name := flags.Arg(0)
	if !*assumeYes {
		if _, err := fmt.Fprintf(output, "remove upstream %q and its repository mappings? type yes: ", name); err != nil {
			return err
		}
		if !readConfirmation(input) {
			_, err := fmt.Fprintln(output, "aborted")
			return err
		}
	}
	if mutationGate == nil {
		return errors.New("admin audit mutation gate is unavailable")
	}
	if err := mutationGate(ctx, "upstream.database.open", *databasePath); err != nil {
		return err
	}
	database, err := store.OpenAdminClient(ctx, *databasePath, store.Options{})
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()
	if err := mutationGate(ctx, "upstream.remove", *databasePath); err != nil {
		return err
	}
	repositories, err := database.RemoveUpstream(ctx, administrativeActor, name)
	if err != nil {
		return err
	}
	if len(repositories) == 0 {
		_, err = fmt.Fprintf(output, "removed upstream %s (no repository mappings)\n", name)
		return err
	}
	_, err = fmt.Fprintf(output, "removed upstream %s and repository mappings: %s\n", name, strings.Join(repositories, ", "))
	return err
}

// readConfirmation consumes one line from input and reports whether it is the
// literal confirmation word.
func readConfirmation(input io.Reader) bool {
	if input == nil {
		return false
	}
	scanner := bufio.NewScanner(input)
	if !scanner.Scan() {
		return false
	}
	return strings.TrimSpace(scanner.Text()) == "yes"
}

// runUplinkList prints every registered uplink with its token-credential
// usage. Listing is read-only and requires no mutation gate.
func runUplinkList(ctx context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("uplink list", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	databasePath := flags.String("database", "/data/oberth.sqlite", "SQLite database path (in-pod; requires the live admin daemon)")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: uplink list accepts no arguments", errUsage)
	}
	database, err := store.OpenAdminClient(ctx, *databasePath, store.Options{})
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()
	uplinks, err := database.ListUplinkStatuses(ctx)
	if err != nil {
		return err
	}
	writer := tabwriter.NewWriter(output, 0, 0, 3, ' ', 0)
	if _, err := fmt.Fprintln(writer, "IDENTITY\tFINGERPRINT\tADMIN\tCREATED\tLAST SEEN"); err != nil {
		return err
	}
	for _, uplink := range uplinks {
		lastSeen := "never"
		if !uplink.LastUsedAt.IsZero() {
			lastSeen = uplink.LastUsedAt.UTC().Format("2006-01-02 15:04")
		}
		adminLabel := "no"
		if uplink.Admin {
			adminLabel = "yes"
		}
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n",
			uplink.Identity, uplink.Fingerprint, adminLabel, uplink.CreatedAt.UTC().Format("2006-01-02 15:04"), lastSeen); err != nil {
			return err
		}
	}
	return writer.Flush()
}

// runUplinkRemove deletes one uplink and revokes its bearer token immediately.
func runUplinkRemove(ctx context.Context, arguments []string, output io.Writer, mutationGate func(context.Context, string, string) error) error {
	flags := flag.NewFlagSet("uplink remove", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	databasePath := flags.String("database", "/data/oberth.sqlite", "SQLite database path (in-pod; requires the live admin daemon)")
	fingerprint := flags.String("fingerprint", "", "disambiguating SSH public-key fingerprint when one identity has several uplinks")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("%w: uplink remove [--fingerprint SHA256:...] <identity>", errUsage)
	}
	identity := flags.Arg(0)
	if mutationGate == nil {
		return errors.New("admin audit mutation gate is unavailable")
	}
	if err := mutationGate(ctx, "uplink.database.open", *databasePath); err != nil {
		return err
	}
	database, err := store.OpenAdminClient(ctx, *databasePath, store.Options{})
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()
	if err := mutationGate(ctx, "uplink.remove", *databasePath); err != nil {
		return err
	}
	value, err := database.RemoveUplink(ctx, administrativeActor, identity, *fingerprint)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "removed uplink %s (%s); its bearer token is revoked\n", value.Identity, value.Fingerprint)
	return err
}

type repoDependencies struct {
	mutationGate func(context.Context, string, string) error
}

func runRepo(ctx context.Context, arguments []string, output io.Writer) error {
	return runRepoWithDependencies(ctx, arguments, output, repoDependencies{mutationGate: requestLiveAdminMutationGate})
}

// runRepoWithDependencies dispatches repository administration subcommands.
func runRepoWithDependencies(ctx context.Context, arguments []string, output io.Writer, dependencies repoDependencies) error {
	if len(arguments) == 0 {
		return fmt.Errorf("%w: repo add|remove|verify", errUsage)
	}
	switch arguments[0] {
	case "add":
		return runRepoAdd(ctx, arguments[1:], output, dependencies)
	case "remove":
		return runRepoRemove(ctx, arguments[1:], output, dependencies)
	case "verify":
		return runRepoVerify(ctx, arguments[1:], output)
	default:
		return fmt.Errorf("%w: repo add|remove|verify", errUsage)
	}
}

// runRepoAdd maps one repository name to a configured upstream. Push-time
// discovery performs this mapping automatically only while exactly one
// upstream is configured; with several upstreams an unmapped push fails
// closed, and this command is the explicit administrative mapping step.
func runRepoAdd(ctx context.Context, arguments []string, output io.Writer, dependencies repoDependencies) error {
	flags := flag.NewFlagSet("repo add", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	databasePath := flags.String("database", "/data/oberth.sqlite", "SQLite database path (in-pod; requires the live admin daemon)")
	defaultBranch := flags.String("default-branch", "main", "repository default branch until the first push confirms it")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 2 {
		return fmt.Errorf("%w: repo add requires a repository name and an upstream name", errUsage)
	}
	name, err := gitcache.NormalizeRepo(flags.Arg(0))
	if err != nil {
		return err
	}
	upstreamName := flags.Arg(1)
	branch := strings.TrimSpace(*defaultBranch)
	if branch == "" || strings.ContainsAny(branch, "\x00\r\n ") {
		return fmt.Errorf("%w: default branch is invalid", errUsage)
	}
	if dependencies.mutationGate == nil {
		return errors.New("admin audit mutation gate is unavailable")
	}
	if err := dependencies.mutationGate(ctx, "repository.database.open", *databasePath); err != nil {
		return err
	}
	database, err := store.OpenAdminClient(ctx, *databasePath, store.Options{})
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()
	upstreams, err := database.ListUpstreams(ctx)
	if err != nil {
		return err
	}
	var upstream model.Upstream
	names := make([]string, 0, len(upstreams))
	for _, candidate := range upstreams {
		names = append(names, candidate.Name)
		if candidate.Name == upstreamName {
			upstream = candidate
		}
	}
	if upstream.ID <= 0 {
		return fmt.Errorf("upstream %q is not registered (configured: %s)", upstreamName, strings.Join(names, ", "))
	}
	if existing, lookupErr := database.RepositoryByName(ctx, name); lookupErr == nil {
		if existing.UpstreamID == upstream.ID {
			_, err = fmt.Fprintf(output, "repository %s is already mapped to upstream %s\n", name, upstream.Name)
			return err
		}
		return fmt.Errorf("repository %s is already mapped to a different upstream (id %d); remapping is not supported", name, existing.UpstreamID)
	} else if !errors.Is(lookupErr, store.ErrNotFound) {
		return lookupErr
	}
	if err := dependencies.mutationGate(ctx, "repository.register", *databasePath); err != nil {
		return err
	}
	value, err := database.RegisterRepository(ctx, administrativeActor, model.RepositorySpec{
		Name: name, UpstreamID: upstream.ID, DefaultBranch: branch,
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "registered repository %s -> upstream %s (%s); the first push proves the mapping and refreshes the default branch\n",
		value.Name, upstream.Name, upstream.BaseURL)
	return err
}

// runRepoVerify probes each registered repository's upstream with
// git ls-remote to verify that the mapping resolves and the forge
// grants read access. A failure here is exactly the error a first push
// would hit (issue #212 part 3): "exit status 128" with no durable
// record. Running verify after repo add catches typos and permission
// issues immediately; running verify --all catches key rotations.
func runRepoVerify(ctx context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("repo verify", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	databasePath := flags.String("database", "/data/oberth.sqlite", "SQLite database path")
	gitCacheRoot := flags.String("git-cache-root", "/data/git", "root directory for bare Git caches")
	all := flags.Bool("all", false, "verify every registered repository")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if !*all && flags.NArg() == 0 {
		return fmt.Errorf("%w: repo verify <repository> | repo verify --all", errUsage)
	}
	database, err := store.OpenAdminClient(ctx, *databasePath, store.Options{})
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()
	upstreams, err := database.ListUpstreams(ctx)
	if err != nil {
		return err
	}
	upstreamMap := make(map[int64]model.Upstream, len(upstreams))
	for _, upstream := range upstreams {
		upstreamMap[upstream.ID] = upstream
	}
	var repos []model.Repository
	if *all {
		repos, err = database.ListRepositories(ctx)
		if err != nil {
			return err
		}
	} else {
		for i := 0; i < flags.NArg(); i++ {
			name, normErr := gitcache.NormalizeRepo(flags.Arg(i))
			if normErr != nil {
				return normErr
			}
			repo, lookupErr := database.RepositoryByName(ctx, name)
			if lookupErr != nil {
				return fmt.Errorf("repository %s: %w", name, lookupErr)
			}
			repos = append(repos, repo)
		}
	}
	if len(repos) == 0 {
		_, _ = fmt.Fprintln(output, "no repositories registered")
		return nil
	}
	// Build a minimal git cache for ls-remote probing. The cache is read-only:
	// it never modifies the actual cache directory.
	resolver := app.Upstreams{Catalog: database}
	cache, cacheErr := gitcache.New(gitcache.Config{
		Root:     *gitCacheRoot,
		Upstream: resolver.Remote,
	})
	if cacheErr != nil {
		return fmt.Errorf("initialize git cache for verification: %w", cacheErr)
	}
	var failures int
	for _, repo := range repos {
		_, ok := upstreamMap[repo.UpstreamID]
		if !ok {
			_, _ = fmt.Fprintf(output, "FAIL  %s: upstream id %d not found\n", repo.Name, repo.UpstreamID)
			failures++
			continue
		}
		remote, resolveErr := resolver.Remote(repo.Name)
		if resolveErr != nil {
			_, _ = fmt.Fprintf(output, "FAIL  %s: cannot resolve upstream: %v\n", repo.Name, resolveErr)
			failures++
			continue
		}
		// Use the cache to run ls-remote with the configured SSH env.
		_, lsErr := cache.LsRemoteHeads(ctx, repo.Name)
		if lsErr != nil {
			_, _ = fmt.Fprintf(output, "FAIL  %s -> %s: %v\n", repo.Name, remote, lsErr)
			failures++
		} else {
			_, _ = fmt.Fprintf(output, "OK    %s -> %s\n", repo.Name, remote)
		}
	}
	if failures > 0 {
		return fmt.Errorf("%d of %d repositories failed verification", failures, len(repos))
	}
	return nil
}

// runRepoRemove deletes a repository mapping and its Git cache directory.
// Refuses if the repository has in-flight runs or pending promotions.
func runRepoRemove(ctx context.Context, arguments []string, output io.Writer, dependencies repoDependencies) error {
	flags := flag.NewFlagSet("repo remove", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	databasePath := flags.String("database", "/data/oberth.sqlite", "SQLite database path (in-pod; requires the live admin daemon)")
	gitCacheRoot := flags.String("git-cache-root", "/data/git", "root directory for bare Git caches")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("%w: repo remove requires a repository name", errUsage)
	}
	name, err := gitcache.NormalizeRepo(flags.Arg(0))
	if err != nil {
		return err
	}
	if dependencies.mutationGate == nil {
		return errors.New("admin audit mutation gate is unavailable")
	}
	if err := dependencies.mutationGate(ctx, "repository.database.open", *databasePath); err != nil {
		return err
	}
	database, err := store.OpenAdminClient(ctx, *databasePath, store.Options{})
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()
	if err := dependencies.mutationGate(ctx, "repository.remove", *databasePath); err != nil {
		return err
	}
	removed, err := database.RemoveRepository(ctx, administrativeActor, name)
	if err != nil {
		return err
	}

	// Clean up the bare Git cache directory.
	cachePath := filepath.Join(*gitCacheRoot, name+".git")
	if err := os.RemoveAll(cachePath); err != nil {
		// Cache cleanup is best-effort: the database record is already gone,
		// so a stale directory is inert and the next push will re-create it.
		_, _ = fmt.Fprintf(output, "warning: failed to remove git cache %s: %v\n", cachePath, err)
	}

	_, err = fmt.Fprintf(output, "removed repository %s (was upstream %s)\n", removed.Name, removed.UpstreamName)
	return err
}

func runUplink(ctx context.Context, arguments []string, input io.Reader, output io.Writer) error {
	return runUplinkWithDependencies(ctx, arguments, input, output, uplinkDependencies{mutationGate: requestLiveAdminMutationGate})
}

func runUplinkWithDependencies(ctx context.Context, arguments []string, input io.Reader, output io.Writer, dependencies uplinkDependencies) error {
	if len(arguments) == 0 {
		return fmt.Errorf("%w: uplink add|list|remove", errUsage)
	}
	switch arguments[0] {
	case "add":
	case "list":
		return runUplinkList(ctx, arguments[1:], output)
	case "remove":
		return runUplinkRemove(ctx, arguments[1:], output, dependencies.mutationGate)
	default:
		return fmt.Errorf("%w: uplink add [--admin] [--database PATH] [--tls-cert PATH] <public-key-file|raw-key|-> <identity@host> | uplink list | uplink remove <identity>", errUsage)
	}
	flags := flag.NewFlagSet("uplink add", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	databasePath := flags.String("database", "/data/oberth.sqlite", "SQLite database path (in-pod; requires the live admin daemon)")
	certificatePath := flags.String("tls-cert", "/etc/oberth/tls/tls.crt", "HTTPS certificate path (in-pod; requires the live admin daemon)")
	isAdmin := flags.Bool("admin", false, "Grant admin privileges (required for access_allow/access_revoke)")
	if err := flags.Parse(arguments[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 2 {
		return fmt.Errorf("%w: uplink add requires one public-key source and identity", errUsage)
	}
	identity := flags.Arg(1)
	if err := app.ValidateUplinkIdentity(identity); err != nil {
		return err
	}
	var fingerprint string
	var err error
	if flags.Arg(0) == "-" {
		fingerprint, err = app.AuthorizedKeyFingerprintReader(input)
	} else if _, statErr := os.Stat(flags.Arg(0)); statErr != nil {
		fingerprint, err = app.AuthorizedKeyFingerprintReader(strings.NewReader(flags.Arg(0)))
	} else {
		fingerprint, err = app.AuthorizedKeyFingerprint(flags.Arg(0))
	}
	if err != nil {
		return err
	}
	if _, err := app.TLSCertificateFingerprint(*certificatePath); err != nil {
		return err
	}
	if dependencies.mutationGate == nil {
		return errors.New("admin audit mutation gate is unavailable")
	}
	gate := func(ctx context.Context, operation string) error {
		return dependencies.mutationGate(ctx, operation, *databasePath)
	}
	if err := gate(ctx, "uplink.database.open"); err != nil {
		return err
	}
	database, err := store.OpenAdminClient(ctx, *databasePath, store.Options{})
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()
	tokens := gatedAdminTokens{Store: database, Gate: gate}
	issuer := auth.Issuer{
		Tokens: tokens,
		Bind: func(ctx context.Context, pending model.TokenCredential) error {
			if err := gate(ctx, "uplink.register"); err != nil {
				return err
			}
			_, err := database.RegisterUplink(ctx, administrativeActor, model.UplinkSpec{
				Fingerprint: fingerprint, Identity: identity, TokenCredentialID: pending.ID,
				Admin: *isAdmin,
			})
			return err
		},
	}
	adminLabel := ""
	if *isAdmin {
		adminLabel = " (admin)"
	}
	if _, err := fmt.Fprintf(output, "Uplink token for %s%s (shown once):\n", identity, adminLabel); err != nil {
		return err
	}
	if _, err := issuer.Issue(ctx, identity, output); err != nil {
		return err
	}
	return nil
}

type gatedAdminTokens struct {
	*store.Store
	Gate func(context.Context, string) error
}

func (tokens gatedAdminTokens) CreateTokenCredential(ctx context.Context, spec model.TokenCredentialSpec) (model.TokenCredential, error) {
	if err := tokens.Gate(ctx, "uplink.token.create"); err != nil {
		return model.TokenCredential{}, err
	}
	return tokens.Store.CreateTokenCredential(ctx, spec)
}

func (tokens gatedAdminTokens) ActivateTokenCredential(ctx context.Context, id string) (model.TokenCredential, error) {
	if err := tokens.Gate(ctx, "uplink.token.activate"); err != nil {
		return model.TokenCredential{}, err
	}
	return tokens.Store.ActivateTokenCredential(ctx, id)
}

func (tokens gatedAdminTokens) TouchTokenCredential(ctx context.Context, id string) error {
	if err := tokens.Gate(ctx, "uplink.token.touch"); err != nil {
		return err
	}
	return tokens.Store.TouchTokenCredential(ctx, id)
}

// runAccess implements the `oberth access` subcommand family for managing the
// secret access approval table.
func runAccess(ctx context.Context, arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return fmt.Errorf("%w: access list|allow|revoke", errUsage)
	}
	switch arguments[0] {
	case "list":
		return runAccessList(ctx, arguments[1:], output)
	case "allow":
		return runAccessAllow(ctx, arguments[1:], output)
	case "revoke":
		return runAccessRevoke(ctx, arguments[1:], output)
	default:
		return fmt.Errorf("%w: access list [--repo NAME] [--revoked] | access allow <repo> <step> <secret> | access revoke <repo> <step> <secret>", errUsage)
	}
}

func runAccessList(ctx context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("access list", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	databasePath := flags.String("database", "/data/oberth.sqlite", "SQLite database path (in-pod; requires the live admin daemon)")
	repo := flags.String("repo", "", "filter by repository name")
	revoked := flags.Bool("revoked", false, "include revoked grants")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: access list accepts no positional arguments", errUsage)
	}
	database, err := store.OpenAdminClient(ctx, *databasePath, store.Options{})
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()
	grants, err := database.SecretAccessList(ctx, *repo, *revoked)
	if err != nil {
		return err
	}
	writer := tabwriter.NewWriter(output, 0, 0, 3, ' ', 0)
	if _, err := fmt.Fprintln(writer, "REPO\tSTEP\tSECRET\tAPPROVED BY\tAPPROVED AT\tSTATUS"); err != nil {
		return err
	}
	for _, grant := range grants {
		status := "active"
		if grant.RevokedAt != nil {
			status = "revoked by " + grant.RevokedBy
		}
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n",
			grant.Repo, grant.Step, grant.Secret,
			grant.ApprovedBy, grant.ApprovedAt.UTC().Format("2006-01-02 15:04"),
			status); err != nil {
			return err
		}
	}
	return writer.Flush()
}

type accessDependencies struct {
	kubernetesClient func() (kubernetes.Interface, error)
	mutationGate     func(context.Context, string, string) error
}

func runAccessAllow(ctx context.Context, arguments []string, output io.Writer) error {
	return runAccessAllowWithDependencies(ctx, arguments, output, accessDependencies{
		kubernetesClient: func() (kubernetes.Interface, error) {
			config, err := rest.InClusterConfig()
			if err != nil {
				return nil, err
			}
			config.Timeout = 10 * time.Second
			return kubernetes.NewForConfig(config)
		},
		mutationGate: requestLiveAdminMutationGate,
	})
}

func runAccessAllowWithDependencies(ctx context.Context, arguments []string, output io.Writer, deps accessDependencies) error {
	flags := flag.NewFlagSet("access allow", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	databasePath := flags.String("database", "/data/oberth.sqlite", "SQLite database path (in-pod; requires the live admin daemon)")
	namespace := flags.String("namespace", defaultPodNamespace(), "Kubernetes namespace")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 3 {
		return fmt.Errorf("%w: access allow <repo> <step> <secret-path>", errUsage)
	}
	repo, step, secret := flags.Arg(0), flags.Arg(1), flags.Arg(2)
	if deps.mutationGate == nil {
		return errors.New("admin audit mutation gate is unavailable")
	}
	if err := deps.mutationGate(ctx, "access.database.open", *databasePath); err != nil {
		return err
	}
	kube, err := deps.kubernetesClient()
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}
	database, err := store.OpenAdminClient(ctx, *databasePath, store.Options{})
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()
	if err := deps.mutationGate(ctx, "access.allow", *databasePath); err != nil {
		return err
	}
	logger := log.New(output, "", 0)
	reconciler := service.NewAccessReconciler(kube, *namespace, database, logger)
	if err := reconciler.UpdateConfigMap(ctx, administrativeActor, service.AddGrant(repo, step, secret)); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "granted: %s/%s may access %s\n", repo, step, secret)
	return err
}

func runAccessRevoke(ctx context.Context, arguments []string, output io.Writer) error {
	return runAccessRevokeWithDependencies(ctx, arguments, output, accessDependencies{
		kubernetesClient: func() (kubernetes.Interface, error) {
			config, err := rest.InClusterConfig()
			if err != nil {
				return nil, err
			}
			config.Timeout = 10 * time.Second
			return kubernetes.NewForConfig(config)
		},
		mutationGate: requestLiveAdminMutationGate,
	})
}

func runAccessRevokeWithDependencies(ctx context.Context, arguments []string, output io.Writer, deps accessDependencies) error {
	flags := flag.NewFlagSet("access revoke", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	databasePath := flags.String("database", "/data/oberth.sqlite", "SQLite database path (in-pod; requires the live admin daemon)")
	namespace := flags.String("namespace", defaultPodNamespace(), "Kubernetes namespace")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 3 {
		return fmt.Errorf("%w: access revoke <repo> <step> <secret-path>", errUsage)
	}
	repo, step, secret := flags.Arg(0), flags.Arg(1), flags.Arg(2)
	if deps.mutationGate == nil {
		return errors.New("admin audit mutation gate is unavailable")
	}
	if err := deps.mutationGate(ctx, "access.database.open", *databasePath); err != nil {
		return err
	}
	kube, err := deps.kubernetesClient()
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}
	database, err := store.OpenAdminClient(ctx, *databasePath, store.Options{})
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()
	if err := deps.mutationGate(ctx, "access.revoke", *databasePath); err != nil {
		return err
	}
	logger := log.New(output, "", 0)
	reconciler := service.NewAccessReconciler(kube, *namespace, database, logger)
	if err := reconciler.UpdateConfigMap(ctx, administrativeActor, service.RemoveGrant(repo, step, secret)); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "revoked: %s/%s no longer has access to %s\n", repo, step, secret)
	return err
}
