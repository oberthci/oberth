package installer

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/kubeconfig"

	"golang.org/x/term"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/mdnsdial"
	"github.com/oberthci/oberth/internal/releaseverify"
)

// CommandRunner executes one host command. Input is connected to stdin when
// non-empty; commands are always invoked directly, never through a shell.
// Failed child commands expose captured stderr through exec.ExitError.Stderr.
type CommandRunner func(ctx context.Context, input []byte, name string, args ...string) ([]byte, error)

// InteractiveRunner connects one host command directly to the operator's
// terminal for pod-side interactive flows (kubectl exec -i). Commands are
// always invoked directly, never through a shell.
type InteractiveRunner func(ctx context.Context, name string, args ...string) error

// InstallDeps holds injectable host and cluster dependencies for the CLI-level
// install orchestration.
type InstallDeps struct {
	installResolved    func(Config)
	ProbeNetworkPolicy func(context.Context, Config, Deps) NetworkPolicyProbeResult
	Output             io.Writer
	Input              io.Reader
	GOOS               string
	LookPath           func(string) (string, error)
	RunCommand         CommandRunner
	RunInteractive     InteractiveRunner
	IsTerminal         func() bool
	// OutputIsTerminal reports whether Output reaches a terminal (default:
	// standard output is one). Credentials are shown only on a terminal;
	// anywhere else they go to --secrets-file or the install refuses.
	OutputIsTerminal func() bool
	LoadKubeConfig   func(contextName string) (kubernetes.Interface, *rest.Config, string, error)
	// KubeContexts lists the contexts of the selected kubeconfig (an explicit
	// path, else KUBECONFIG, else ~/.kube/config). Without --context an
	// install needs exactly one; nil skips the check (tests that inject
	// LoadKubeConfig).
	KubeContexts func(kubeconfigPath string) ([]string, error)
	RunHelm      func(ctx context.Context, args []string) ([]byte, error)
	// CredentialSink, when non-nil, receives every held once-only credential
	// (OpenBao root token, unseal keys, uplink bearer token) as structured
	// label/value pairs INSTEAD of the boxed terminal flush. The TUI wizard
	// sets it: its Output is a log surface (tail + retained lines), and a
	// secret value must never transit a log-shaped text stream — it must
	// reach the masker and the shown-once ceremony as data, not as text to
	// be re-parsed out of box-drawing characters.
	CredentialSink func(label, value string)
	// StepProgressSink, when non-nil, receives structured step completion
	// events for TUI progress tracking. Step names are stable identifiers
	// matched by the TUI's stepNameToIndex map.
	StepProgressSink func(step, status string)
	UplinkCreated    func(identity string)
	upstreamResult   func(InstallUpstream)
	upstream         *InstallUpstream
	secretsSaved     func(keys []string)
	// ReleaseGetter fetches the signed release (default: HTTPS).
	ReleaseGetter releaseverify.Getter
	// releaseKey replaces the compiled-in release key in tests.
	releaseKey *ecdsa.PublicKey
}

func (deps InstallDeps) withDefaults() InstallDeps {
	if deps.Output == nil {
		deps.Output = io.Discard
	}
	if deps.Input == nil {
		deps.Input = os.Stdin
	}
	if deps.GOOS == "" {
		deps.GOOS = runtime.GOOS
	}
	if deps.LookPath == nil {
		deps.LookPath = exec.LookPath
	}
	if deps.RunCommand == nil {
		deps.RunCommand = DefaultRunCommand
	}
	if deps.RunInteractive == nil {
		deps.RunInteractive = DefaultRunInteractive
	}
	if deps.IsTerminal == nil {
		deps.IsTerminal = StdinIsTerminal
	}
	if deps.OutputIsTerminal == nil {
		deps.OutputIsTerminal = func() bool { return term.IsTerminal(int(os.Stdout.Fd())) }
	}
	if deps.LoadKubeConfig == nil {
		if deps.KubeContexts == nil {
			deps.KubeContexts = KubeconfigContexts
		}
		output := deps.Output
		deps.LoadKubeConfig = func(contextName string) (kubernetes.Interface, *rest.Config, string, error) {
			return LoadKubeConfigForContext(contextName, output)
		}
	}
	if deps.RunHelm == nil {
		deps.RunHelm = DefaultRunHelm
	}
	return deps
}

// CheckHostTools reports the host binaries every install path drives but that
// are missing from PATH: helm installs the charts and kubectl runs every
// in-cluster step. Checked before anything is installed, so a missing tool
// stops the install up front instead of at its first Helm step.
func CheckHostTools(goos string, lookPath func(string) (string, error)) error {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	hints := map[string]string{
		"helm":    "https://helm.sh/docs/intro/install/",
		"kubectl": "https://kubernetes.io/docs/tasks/tools/",
	}
	if goos == "darwin" {
		hints["helm"] = "`brew install helm`"
		hints["kubectl"] = "`brew install kubernetes-cli`"
	}
	var missing []string
	for _, tool := range []string{"helm", "kubectl"} {
		if _, err := lookPath(tool); err != nil {
			missing = append(missing, fmt.Sprintf("%s not found in PATH; install it (%s)", tool, hints[tool]))
		}
	}
	if len(missing) > 0 {
		return errors.New(strings.Join(missing, "\n") + "\nthen rerun oberth")
	}
	return nil
}

// Execute prepares the platform-selected Kubernetes cluster, loads its
// kubeconfig, and runs the cluster install flow.
func Execute(ctx context.Context, cfg Config, deps InstallDeps) error {
	deps = deps.withDefaults()
	defer func() { clear(cfg.resumeRootToken) }()
	if err := prepareInstallInputs(ctx, &cfg, deps); err != nil {
		return err
	}

	if err := cfg.Validate(); err != nil {
		return err
	}
	// A dry run only prints the plan, so it stays usable on a host that has
	// not installed the tools yet.
	if !cfg.DryRun {
		if err := CheckHostTools(deps.GOOS, deps.LookPath); err != nil {
			return err
		}
	}

	return executeWithCredentials(ctx, cfg, deps)
}

func executeWithCredentials(ctx context.Context, cfg Config, deps InstallDeps) (resultErr error) {
	// Credentials shown once never go into a log or a transcript: without a
	// terminal they go to --secrets-file, and without one the install refuses
	// before anything exists that could be lost.
	secretsFileSink := false
	if !cfg.DryRun && deps.CredentialSink == nil {
		switch {
		case cfg.SecretsFile != "":
			secrets, err := createSecretsFile(cfg.SecretsFile)
			if err != nil {
				return err
			}
			if cfg.SecretsAgeRecipient != "" {
				if err := secrets.enableEncryption(ctx, cfg.SecretsAgeRecipient); err != nil {
					secrets.finish(io.Discard)
					return err
				}
			}
			cfg.SecretsFile = secrets.path
			deps.CredentialSink = secrets.add
			secretsFileSink = true
			defer func() {
				secrets.finish(deps.Output)
				if secrets.err == nil && secrets.written > 0 && deps.secretsSaved != nil {
					deps.secretsSaved(secrets.keys)
				}
				if secrets.err != nil {
					resultErr = errors.New("credentials could not all be saved; recover the store before continuing")
				}
			}()
		case !deps.OutputIsTerminal() && cfg.mayCreateCredentials(deps.IsTerminal != nil && deps.IsTerminal()):
			return errors.New("this install can create credentials that are shown once (the OpenBao root token and unseal key, " +
				"the first uplink's bearer token), and its output is not a terminal: pass --secrets-file PATH " +
				"(a new file outside any git worktree, written 0600; the output names only its path and SHA-256)")
		}
	} else if cfg.SecretsFile != "" && !cfg.DryRun {
		return errors.New("--secrets-file cannot be combined with a caller that receives the credentials itself")
	}

	return executeCluster(ctx, cfg, deps, secretsFileSink)
}

func executeCluster(ctx context.Context, cfg Config, deps InstallDeps, secretsFileSink bool) error {
	preflightRun := deps.RunCommand
	contextName := cfg.ContextName

	if deps.KubeContexts != nil {
		if err := RequireContextChoice("install on", contextName, cfg.KubeconfigPath, deps.KubeContexts); err != nil {
			return err
		}
	}

	if cfg.KubeconfigPath != "" {
		deps.LoadKubeConfig = func(name string) (kubernetes.Interface, *rest.Config, string, error) {
			rules := kubeconfig.Rules("")
			rules.ExplicitPath = cfg.KubeconfigPath
			return loadKubeConfigWithRules(rules, name, "", deps.Output)
		}
		originalInteractive := deps.RunInteractive
		deps.RunInteractive = func(ctx context.Context, name string, args ...string) error {
			if name == "kubectl" {
				args = append([]string{"--kubeconfig", cfg.KubeconfigPath}, args...)
			}
			return originalInteractive(ctx, name, args...)
		}
		originalRun := deps.RunCommand
		path := cfg.KubeconfigPath
		deps.RunCommand = func(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
			if name == "kubectl" {
				args = append([]string{"--kubeconfig", path}, args...)
			}
			return originalRun(ctx, input, name, args...)
		}
	}
	kubeClient, restConfig, selectedContext, err := deps.LoadKubeConfig(contextName)
	if err != nil {
		if contextName != "" {
			return fmt.Errorf("load kubeconfig context %q: %w", contextName, err)
		}
		return fmt.Errorf("load kubeconfig: %w", err)
	}

	cfg.ContextName = selectedContext
	if !cfg.DryRun {
		preflight := InstallResult{OK: true}
		identity := publicClusterIdentity(cfg, restConfig, selectedContext)
		if !checkClusterTrust(restConfig, identity, &preflight) {
			return fmt.Errorf("%w: cluster_ca: %s", errInstallPreflight, preflight.Checks[0].Remedy)
		}
		preflightConfig := cfg
		// Preflight builds its own explicit kubeconfig arguments.
		inspectInstallPreflight(ctx, &preflightConfig, Deps{KubeClient: kubeClient, RestConfig: restConfig, ContextName: selectedContext, RunCommand: preflightRun, ProbeNetworkPolicy: deps.ProbeNetworkPolicy}, deps, selectedContext, &ClusterIdentity{}, &preflight)
		cfg = preflightConfig
		if !preflight.OK {
			var failures []string
			for _, check := range preflight.Checks {
				if !check.OK && !check.Warning {
					failures = append(failures, check.Code+": "+check.Remedy)
				}
			}
			return fmt.Errorf("%w:\n%s", errInstallPreflight, strings.Join(failures, "\n"))
		}
		if cfg.existingRelease {
			for _, check := range preflight.Checks {
				if check.Code == "network_policy" {
					_, _ = fmt.Fprintf(deps.Output, "NetworkPolicy capability: %s (information during maintenance). %s\n", check.Reason, check.Remedy)
				}
			}
		}

	}

	if deps.installResolved != nil {
		deps.installResolved(cfg)
	}
	PrintReview(deps.Output, cfg)
	// Wire the production ReadPassword and MakeRaw defaults when the session
	// is interactive: x/term.ReadPassword and x/term.MakeRaw on the stdin fd.
	// Non-interactive sessions leave both nil (the callers check isInteractive
	// or fall back to text-based input).
	var readPw func() ([]byte, error)
	var makeRaw func() (func(), error)
	if deps.IsTerminal != nil && deps.IsTerminal() {
		readPw = func() ([]byte, error) {
			return term.ReadPassword(int(os.Stdin.Fd()))
		}
		makeRaw = func() (func(), error) {
			oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
			if err != nil {
				return nil, err
			}
			return func() { _ = term.Restore(int(os.Stdin.Fd()), oldState) }, nil
		}
	}

	// Top-level terminal restore safety net: if a panic or unhandled signal
	// occurs while the terminal is in raw mode, this deferred restore
	// ensures the terminal is returned to its previous state on any exit
	// path from Execute. The wrapper tracks the latest restore function
	// returned by MakeRaw so the defer always calls the right one.
	var terminalRestore func()
	if makeRaw != nil {
		originalMakeRaw := makeRaw
		makeRaw = func() (func(), error) {
			restore, err := originalMakeRaw()
			if err != nil {
				return restore, err
			}
			terminalRestore = restore
			return restore, nil
		}
	}
	defer func() {
		if terminalRestore != nil {
			terminalRestore()
		}
	}()

	// Resolve helm kubeconfig targeting so every helm invocation in the
	// install flow targets the same cluster the binary resolved (#464).
	var helmKubeArgs []string
	if cfg.KubeconfigPath != "" {
		helmKubeArgs = []string{"--kubeconfig", cfg.KubeconfigPath, "--kube-context", selectedContext}
	} else {
		helmKubeArgs = []string{"--kube-context", selectedContext}
	}
	runHelm := HelmWithKubeArgs(deps.RunHelm, helmKubeArgs)

	return Run(ctx, cfg, Deps{
		Output:            deps.Output,
		Input:             deps.Input,
		RunHelm:           runHelm,
		RunCommand:        deps.RunCommand,
		RunInteractive:    deps.RunInteractive,
		IsTerminal:        deps.IsTerminal,
		KubeClient:        kubeClient,
		RestConfig:        restConfig,
		ContextName:       selectedContext,
		ReadPassword:      readPw,
		MakeRaw:           makeRaw,
		CredentialSink:    deps.CredentialSink,
		credentialsAtOnce: secretsFileSink,
		StepProgressSink:  deps.StepProgressSink,
		UplinkCreated:     deps.UplinkCreated,
		upstreamResult:    deps.upstreamResult,
		ReleaseGetter:     deps.ReleaseGetter,
		releaseKey:        deps.releaseKey,
	})
}

func prepareInstallInputs(ctx context.Context, cfg *Config, deps InstallDeps) error {
	if cfg.ResumeAgeIdentity != "" && cfg.ResumeSecretsFile == "" {
		return errors.New("--resume-age-identity requires --resume-secrets-file")
	}
	if cfg.ResumeSecretsFile != "" {
		if !cfg.InstallSecretStore {
			return errors.New("--resume-secrets-file requires --install-secretstore")
		}
		if !cfg.DryRun {
			token, err := readResumeRootToken(ctx, cfg.ResumeSecretsFile, cfg.ResumeAgeIdentity)
			if err != nil {
				return err
			}
			cfg.resumeRootToken = token
		}
	}

	if cfg.AdminCSRPath != "" {
		if cfg.AdminSSHPublicKeyPath == "" {
			return errors.New("--admin-csr requires --admin-ssh-public-key")
		}
		file, err := os.Open(cfg.AdminCSRPath)
		if err != nil {
			return errors.New("cannot open admin CSR")
		}
		raw, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
		_ = file.Close()
		if err != nil {
			return errors.New("cannot read admin CSR")
		}
		if _, err := clientcert.ParseCSR(raw); err != nil {
			return err
		}
		cfg.adminCSR = raw
		if !cfg.DryRun {
			if _, err := os.Lstat(cfg.AdminCSRPath + ".crt"); err == nil {
				return errors.New("admin certificate output already exists; choose a new CSR path")
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	if cfg.AdminBearer && cfg.AdminCSRPath == "" {
		return errors.New("--admin-bearer requires --admin-csr")
	}

	if cfg.AdminSSHPublicKeyPath != "" {
		if cfg.SSHPublicKeyPath != "" && cfg.SSHPublicKeyPath != cfg.AdminSSHPublicKeyPath {
			return errors.New("choose --admin-ssh-public-key or --ssh-public-key")
		}
		var key []byte
		var err error
		if cfg.AdminSSHPublicKeyPath == "-" {
			key, err = io.ReadAll(io.LimitReader(deps.Input, 16385))
		} else {
			var file *os.File
			file, err = os.Open(cfg.AdminSSHPublicKeyPath)
			if err == nil {
				key, err = io.ReadAll(io.LimitReader(file, 16385))
				_ = file.Close()
			}
		}
		if err != nil || len(key) > 16384 || validatePublicKeyFile(key) != nil {
			return errors.New("admin SSH key must be one valid public key (maximum 16 KiB); private keys are refused")
		}
		cfg.adminSSHPublicKey = key
		cfg.SSHPublicKeyPath = cfg.AdminSSHPublicKeyPath
	}
	if cfg.SecretsAgeRecipient != "" {
		if cfg.SecretsFile == "" {
			return errors.New("--secrets-age-recipient requires --secrets-file")
		}
		if err := checkAgeRecipient(ctx, cfg.SecretsAgeRecipient); err != nil {
			return err
		}
	}
	return nil
}

// RequireContextChoice refuses to act on a kubeconfig with several contexts
// when none was named: the operator picks the cluster, a command never does.
// action completes "choose the cluster to ..." (install on, upgrade). A
// kubeconfig that cannot be listed is left to the load that follows.
func RequireContextChoice(action, contextName, kubeconfigPath string, list func(string) ([]string, error)) error {
	if contextName != "" {
		return nil
	}
	names, err := list(kubeconfigPath)
	if err != nil || len(names) < 2 {
		return nil
	}
	return fmt.Errorf("the kubeconfig has %d contexts; choose the cluster to %s with --context <name>:\n  %s",
		len(names), action, strings.Join(names, "\n  "))
}

// KubeconfigContexts lists the context names of a kubeconfig, sorted: the
// file at path, else what KUBECONFIG names, else ~/.kube/config. A kubeconfig
// that cannot be read lists none, and the load that follows reports why.
func KubeconfigContexts(path string) ([]string, error) {
	rules := kubeconfig.Rules("")
	rules.ExplicitPath = path
	raw, err := kubeconfig.Read(rules)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(raw.Contexts))
	for name := range raw.Contexts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// certificateNamesForLoopback covers a selected local client endpoint.
// Existing operator names are preserved and never duplicated.
func certificateNamesForLoopback(dnsNames, ips []string) ([]string, []string) {
	return appendMissing(dnsNames, "localhost"), appendMissing(appendMissing(ips, "127.0.0.1"), "::1")
}

// certificateNamesForClientHost adds the address host-side clients are told
// to use: localhost (with 127.0.0.1) for a local API server, otherwise the API
// server's own host, as an IP or a DNS name.
func certificateNamesForClientHost(host string, dnsNames, ips []string) ([]string, []string) {
	switch {
	case host == "" || host == "localhost":
		return certificateNamesForLoopback(dnsNames, ips)
	case net.ParseIP(host) != nil:
		return dnsNames, appendMissing(ips, host)
	default:
		return appendMissing(dnsNames, host), ips
	}
}

// clientHostNameRegexp is a DNS name a client can connect to and a
// certificate can carry.
var clientHostNameRegexp = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// NormalizeClientHost checks an address clients are told to use (#061) and
// returns its canonical form: a lower-case DNS name or a normalized IP. An
// unspecified address (0.0.0.0, ::) is refused with the reason: it is where a
// server listens, not where a client connects, and NodePorts already listen
// on every address of the node.
func NormalizeClientHost(raw string) (string, error) {
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		if ip.IsUnspecified() {
			return "", fmt.Errorf("%s is a listen address, not one a client can connect to; NodePorts 30022 and 30443 already listen on every address of the node, so name the node's IP or DNS name", raw)
		}
		return ip.String(), nil
	}
	if len(host) > 253 || !clientHostNameRegexp.MatchString(host) {
		return "", fmt.Errorf("%q is neither an IP address nor a DNS name (no scheme, port or path)", raw)
	}
	return host, nil
}

// LocalNameWarning says what a .local client host costs (#066), or "" for
// any other host: other machines resolve it over mDNS, and avahi also
// advertises container bridges such as k3s's cni0 (10.42.x), which no other
// machine reaches.
func LocalNameWarning(host string) string {
	if !mdnsdial.IsLocalName(host) {
		return ""
	}
	return host + " is an mDNS name: other machines resolve it through avahi, which may also advertise this node's container bridge address (k3s cni0, 10.42.x) that they cannot reach. The node's LAN IP avoids both."
}

// URLHost is host as it goes in a URL authority: an IPv6 literal in
// brackets, anything else unchanged.
func URLHost(host string) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return "[" + host + "]"
	}
	return host
}

// isLoopbackHost reports whether a client host names this machine only.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func appendMissing(values []string, want string) []string {
	for _, value := range values {
		if value == want {
			return values
		}
	}
	return append(values, want)
}

type oberthChartValues struct {
	Image struct {
		Ref string `json:"ref"`
	} `json:"image"`
}

// splitImageDigestRef splits repo[@sha256:digest] and reports whether the ref
// is digest-pinned.
func splitImageDigestRef(ref string) (repo, digest string, ok bool) {
	repo, digest, found := strings.Cut(ref, "@")
	if !found || !strings.HasPrefix(digest, "sha256:") {
		return ref, "", false
	}
	return repo, digest, true
}

func isSHA256Digest(digest string) bool {
	rest, found := strings.CutPrefix(digest, "sha256:")
	if !found || len(rest) != 64 {
		return false
	}
	for _, r := range rest {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func oberthChartValuesArgs(cfg Config) []string {
	// --chart means this deployment's chart is on disk, so its values are read
	// from there. Reading the published chart instead makes --chart a half
	// promise, and fails outright when the binary's version is not a published
	// chart version, which is true of every fork build.
	return []string{"show", "values", oberthChart(cfg)}
}

func commandOutputSuffix(output []byte) string {
	text := strings.TrimSpace(string(output))
	if text == "" {
		return ""
	}
	return ": " + text
}

// DefaultRunCommand runs a host command without a shell and returns its
// combined output for prerequisite and registry diagnostics. Failed child
// commands retain stderr separately in exec.ExitError.
func DefaultRunCommand(ctx context.Context, input []byte, name string, args ...string) (result []byte, resultErr error) {
	args, environment, lease, cleanup, prepareErr := kubeconfig.Command(name, args, installerChildEnvironment())
	if prepareErr != nil {
		return nil, prepareErr
	}
	defer func() { resultErr = errors.Join(resultErr, cleanup()) }()

	// #nosec G204 -- callers select a fixed executable and pass structured
	// arguments; no shell interpolation occurs.
	command := exec.CommandContext(ctx, name, args...)
	command.Env = environment
	if lease != nil {
		command.ExtraFiles = []*os.File{lease}
	}
	if len(input) > 0 {
		command.Stdin = bytes.NewReader(input)
	}
	var capture commandCapture
	command.Stdout = commandCaptureWriter{capture: &capture}
	command.Stderr = commandCaptureWriter{capture: &capture, stderr: true}
	err := command.Run()
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		exitError.Stderr = capture.stderr.Bytes()
	}
	if err != nil && ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
	}
	return capture.combined.Bytes(), err
}

// Capture both streams as their chunks arrive rather than appending all stderr
// after stdout. Separate os/exec copy goroutines can write concurrently.
type commandCapture struct {
	mu       sync.Mutex
	combined bytes.Buffer
	stderr   bytes.Buffer
}

type commandCaptureWriter struct {
	capture *commandCapture
	stderr  bool
}

func (w commandCaptureWriter) Write(p []byte) (int, error) {
	w.capture.mu.Lock()
	defer w.capture.mu.Unlock()
	if w.stderr {
		_, _ = w.capture.stderr.Write(p) // bytes.Buffer.Write never returns an error.
	}
	return w.capture.combined.Write(p)
}

// DefaultRunInteractive runs a host command wired to the operator's own
// terminal, so pod-side interactive flows (kubectl exec -i ... oberth
// upstream add) can prompt, stream progress, and read answers directly.
func DefaultRunInteractive(ctx context.Context, name string, args ...string) (resultErr error) {
	args, environment, lease, cleanup, prepareErr := kubeconfig.Command(name, args, installerChildEnvironment())
	if prepareErr != nil {
		return prepareErr
	}
	defer func() { resultErr = errors.Join(resultErr, cleanup()) }()

	// #nosec G204 -- callers select a fixed executable and pass structured
	// arguments; no shell interpolation occurs.
	command := exec.CommandContext(ctx, name, args...)
	command.Env = environment
	if lease != nil {
		command.ExtraFiles = []*os.File{lease}
	}
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return errors.Join(command.Run(), ctx.Err())
}

// StdinIsTerminal reports whether stdin is an interactive terminal. The
// isatty check (not a char-device stat, which /dev/null would pass) keeps
// piped and CI runs on the printed manual-steps path.
func StdinIsTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// DefaultRunCommandStream streams wait progress without a shell or buffered
// output. The explicit caller context owns termination; WaitDelay bounds joins
// if an inherited pipe remains open after the local child exits.
func DefaultRunCommandStream(ctx context.Context, output io.Writer, name string, args ...string) (resultErr error) {
	args, environment, lease, cleanup, prepareErr := kubeconfig.Command(name, args, installerChildEnvironment())
	if prepareErr != nil {
		return prepareErr
	}
	defer func() { resultErr = errors.Join(resultErr, cleanup()) }()

	// #nosec G204 -- fixed executable and structured arguments, as in DefaultRunCommand.
	command := exec.CommandContext(ctx, name, args...)
	command.Env = environment
	if lease != nil {
		command.ExtraFiles = []*os.File{lease}
	}
	command.Stdout, command.Stderr = output, output
	command.WaitDelay = 5 * time.Second
	return errors.Join(command.Run(), ctx.Err())
}
