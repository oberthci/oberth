package installer

// First-run onboarding. A fresh Oberth deployment is deliberately NotReady
// until its first upstream is registered — the vault-pre-init pattern: the
// pod runs and accepts in-pod administration while the readiness probe keeps
// it out of Service endpoints. The install therefore never waits blindly for
// Ready. It waits for a Running pod, then either confirms an existing
// upstream, or — on an interactive terminal — walks the operator through
// upstream (deploy key) and uplink (bearer token) setup by driving the
// product's own in-pod `oberth upstream add` / `oberth uplink add` commands
// over kubectl exec, and only then waits for readiness.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// ErrInterrupted is returned when the user presses Ctrl+C (sends ETX / 0x03)
// during an interactive prompt. Callers should propagate it to produce a clean
// exit without an error dump.
var ErrInterrupted = errors.New("interrupted")

// ErrOnboardingPartial is returned when the install completed partially:
// the deployment is up but one or more onboarding steps require manual
// action (e.g., deploying the deploy key at the forge). Callers should
// NOT treat this as a full failure — the installer did what it could and
// the remaining steps are documented in the output.
var ErrOnboardingPartial = errors.New("onboarding partial: manual steps remain")

const (
	// oberthWebUIURL is where the chart's fixed HTTPS NodePort (30443)
	// serves the web UI on a local cluster.
	oberthWebUIURL       = "https://localhost:30443/runs"
	defaultUplinkKeyPath = "~/.ssh/id_ed25519.pub"
)

// FinishInstall completes the install after the Oberth chart is deployed.
// The table writer is still open (has a bottom border drawn); onboarding
// results are appended to the table via AppendRow so the table grows in
// place. Credentials are flushed after the table.
func FinishInstall(ctx context.Context, cfg Config, deps Deps, tw *tableWriter, creds *heldCredentials) error {
	ns := cfg.Namespace
	if ns == "" {
		ns = DefaultNamespace
	}
	if err := waitForPodRunning(ctx, deps, ns, "app.kubernetes.io/instance=oberth", cfg.Timeout); err != nil {
		return fmt.Errorf("wait for Oberth pod: %w", err)
	}
	if deps.StepProgressSink != nil {
		deps.StepProgressSink("rollout ready", "done")
	}

	configured, err := upstreamConfigured(ctx, cfg, deps)
	if err != nil {
		_, _ = fmt.Fprintf(deps.Output, "Could not determine the upstream state (%v); finish the setup manually:\n", err)
		_ = PrintNextSteps(cfg, deps.Output)
		return ErrOnboardingPartial
	}
	if configured {
		// An upstream already exists, so there is nothing to onboard. The
		// client offer still belongs here: it is about this machine, not about
		// the deployment, and a second machine reaches a server that is already
		// set up.
		if err := offerClientAccessToConfiguredDeployment(ctx, cfg, deps, tw); err != nil {
			return err
		}
		return waitAndAnnounceReady(ctx, cfg, deps)
	}

	if !isInteractive(deps) && !cfg.hasOnboardingConfig() {
		_, _ = fmt.Fprintln(deps.Output, "The pod stays NotReady until an upstream is registered. Finish the setup manually:")
		_ = PrintNextSteps(cfg, deps.Output)
		return ErrOnboardingPartial
	}
	return runOnboarding(ctx, cfg, deps, tw, creds)
}

func isInteractive(deps Deps) bool {
	return deps.IsTerminal != nil && deps.IsTerminal() && deps.Input != nil && deps.RunInteractive != nil
}

// hasOnboardingConfig reports whether the Config carries pre-filled
// onboarding data (from the TUI wizard), so a non-interactive session can
// run onboarding without prompting.
func (cfg Config) hasOnboardingConfig() bool {
	return cfg.ForgeURL != "" && cfg.UplinkIdentity != "" && cfg.SSHPublicKeyPath != ""
}

func waitAndAnnounceReady(ctx context.Context, cfg Config, deps Deps) error {
	_, _ = fmt.Fprintln(deps.Output, "Waiting for Oberth to become ready...")
	if err := WaitForReady(ctx, cfg, deps); err != nil {
		return err
	}
	color := isColor(deps)
	if color {
		_, _ = fmt.Fprintf(deps.Output, "\n\033[1;32mReady\033[0m — %s\n", oberthWebUIURL)
	} else {
		_, _ = fmt.Fprintf(deps.Output, "\nReady — %s\n", oberthWebUIURL)
	}
	return nil
}

// kubectlOberthArgs builds kubectl-exec argv for one in-pod oberth command
// against deploy/oberth. stdinAttached adds -i for commands that read stdin.
func kubectlOberthArgs(cfg Config, deps Deps, stdinAttached bool, command ...string) []string {
	ns := cfg.Namespace
	if ns == "" {
		ns = DefaultNamespace
	}
	args := []string{"exec"}
	if stdinAttached {
		args = append(args, "-i")
	}
	if deps.ContextName != "" {
		args = append(args, "--context", deps.ContextName)
	}
	args = append(args, "-c", "oberth", "-n", ns, "deploy/oberth", "--", "oberth")
	return append(args, command...)
}

// upstreamConfigured reports whether at least one upstream is registered, by
// listing them in-pod. Right after the pod turns Running the exec subresource
// and the admin database may need a moment, so transient failures are
// retried briefly.
func upstreamConfigured(ctx context.Context, cfg Config, deps Deps) (bool, error) {
	run := deps.RunCommand
	if run == nil {
		run = DefaultRunCommand
	}
	var out []byte
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(pollInterval(deps)):
			}
		}
		out, err = run(ctx, nil, "kubectl", kubectlOberthArgs(cfg, deps, false, "upstream", "list")...)
		if err == nil {
			return upstreamListHasRows(out), nil
		}
	}
	return false, fmt.Errorf("list upstreams in pod: %w%s", err, commandOutputSuffix(out))
}

// upstreamListHasRows parses `oberth upstream list` output: a tab-separated
// table whose header starts with NAME; any further non-empty line is a
// configured upstream. Lines before the header are ignored — they are kubectl
// stderr noise (e.g., "Defaulted container..." from multi-container pods)
// merged by CombinedOutput.
func upstreamListHasRows(out []byte) bool {
	headerSeen := false
	for _, line := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "NAME") {
			headerSeen = true
			continue
		}
		if headerSeen {
			return true
		}
	}
	return false
}

// runOnboarding drives the interactive first-run setup. Results are appended
// to the table via tw.AppendRow so the table grows in place while prompts
// appear below the bottom border.
func runOnboarding(ctx context.Context, cfg Config, deps Deps, tw *tableWriter, creds *heldCredentials) error {
	w := deps.Output
	color := isColor(deps)

	// --- Upstream URL ---
	baseURL, name, ok, err := promptUpstream(ctx, cfg, deps, tw)
	if err != nil {
		return err
	}
	if !ok {
		_, _ = fmt.Fprintln(w, "No upstream given — skipping onboarding. Finish the setup manually:")
		_ = PrintNextSteps(cfg, w)
		return ErrOnboardingPartial
	}

	// --- Deploy key ---
	keyProvided, err := promptDeployKey(ctx, cfg, deps, tw)
	if err != nil {
		return err
	}

	// --- Register upstream (quiet, with result as table row) ---
	registered, deployPubKey, err := registerUpstreamQuiet(ctx, cfg, deps, name, baseURL)
	if err != nil {
		return err
	}
	tw.AppendRow("Upstream", hostnameFromURL(baseURL), "✓ connected", false)
	if deps.StepProgressSink != nil {
		deps.StepProgressSink("upstream discovery", "done")
	}

	// Show the generated public key as a span row so the user can
	// copy it to register at their forge.
	if deployPubKey != "" && !keyProvided {
		tw.AppendSpanRow(deployPubKey)
	}

	// --- Key registration verification ---
	// The upstream row only exists after a run of `upstream add` whose
	// authentication probe succeeded (the incomplete bootstrap path deliberately
	// registers nothing), so each verification attempt must RERUN the add —
	// exactly the rerun the pod-side flow instructs. Merely re-listing
	// upstreams can never observe the registration.
	keyPending := false
	if registered {
		tw.AppendRow("Key registration", hostnameFromURL(baseURL), "✓ accepted", false)
	} else if !isInteractive(deps) {
		// Non-interactive (TUI wizard): the deploy key was just generated
		// and needs manual registration at the forge. Skip the
		// verification loop — the operator registers it after setup.
		tw.AppendRow("Key registration", hostnameFromURL(baseURL), "pending", false)
		keyPending = true
	} else {
		forgeHost := hostnameFromURL(baseURL)
		var lastAttemptErr error
		for attempt := 0; ; attempt++ {
			startPrompt(w, color, "Key registration", forgeHost+" — register key, Enter to verify: ")
			if _, readErr := readLine(ctx, deps.Input); readErr != nil {
				return fmt.Errorf("read key-registration confirmation: %w", readErr)
			}
			var attemptErr error
			registered, _, attemptErr = registerUpstreamQuiet(ctx, cfg, deps, name, baseURL)
			if registered {
				// Erase the prompt line, then append the result to the table.
				erasePromptLines(w, color, 1)
				tw.AppendRow("Key registration", forgeHost, "✓ accepted", false)
				break
			}
			if attemptErr != nil {
				// Unexpected probe failure — a knownhosts mismatch here is an
				// active MITM indicator; surface it instead of a silent retry
				// (allowlist classification per issue #89).
				lastAttemptErr = attemptErr
				completePrompt(w, color, "Key registration", forgeHost, "⚠ error")
				_, _ = fmt.Fprintf(w, "  ⚠ %s\n", terseInstallError(attemptErr))
			} else {
				completePrompt(w, color, "Key registration", forgeHost, "⚠ not yet")
			}
			if attempt >= 2 {
				tw.AppendRow("Key registration", forgeHost, "✗ failed", false)
				_, _ = fmt.Fprintln(w, "Upstream key not accepted after 3 attempts. Finish the setup manually:")
				if lastAttemptErr != nil {
					_, _ = fmt.Fprintf(w, "Last verification error: %s\n", terseInstallError(lastAttemptErr))
				}
				_ = PrintNextSteps(cfg, w)
				return ErrOnboardingPartial
			}
		}
	}

	// --- Uplink ---
	pubKeyPath, identity, token, uplinkOutput, uplinkErr := onboardUplink(ctx, cfg, deps, tw)
	if uplinkErr != nil {
		ns := cfg.Namespace
		if ns == "" {
			ns = DefaultNamespace
		}
		_, _ = fmt.Fprintf(w, "Uplink setup did not complete (%v).\nAdd one later with:\n\n    kubectl exec -i -n %s deploy/oberth -- oberth uplink add - you@host < ~/.ssh/id_ed25519.pub\n\n", uplinkErr, ns)
		if deps.StepProgressSink != nil {
			deps.StepProgressSink("mint uplink", "pending")
		}
		return ErrOnboardingPartial
	}
	tw.AppendRow("Uplink", identity, "✓ registered", false)
	if deps.StepProgressSink != nil {
		deps.StepProgressSink("mint uplink", "done")
	}

	// --- SSH config ---
	if err := offerSSHConfig(ctx, deps, pubKeyPath, tw); err != nil {
		return err
	}
	sshConfigured := sshHostBlockExists()

	// Collect the bearer token into the deferred credentials pool.
	// When extraction fails, print the sanitized pod output instead —
	// the token is shown exactly once and silently discarding an
	// unrecognized format would lose it forever.
	if token != "" {
		creds.add("Bearer token", token)
	} else if strings.TrimSpace(uplinkOutput) != "" {
		_, _ = fmt.Fprintln(w, strings.TrimRight(uplinkOutput, "\n"))
	}

	// Offered before the credential box is flushed, so the token is still the
	// last thing on screen when the operator is told where to put it.
	if err := offerClientAccess(ctx, cfg, deps, tw, token); err != nil {
		return err
	}

	// Release credentials after the table is fully closed — through the
	// structured sink when one is wired (TUI), else the boxed flush.
	emitCredentials(creds, deps, w, color)

	// When the deploy key is pending (non-interactive: the forge has not
	// accepted it yet), the server stays NotReady until the first upstream
	// probe succeeds. Waiting 5 minutes for that here would block the
	// wizard with no user action possible. Skip WaitForReady and return a
	// partial-success sentinel so the done page shows what remains.
	if keyPending {
		if deps.StepProgressSink != nil {
			deps.StepProgressSink("upstream discovery", "pending")
			deps.StepProgressSink("audit genesis", "pending")
		}
		_, _ = fmt.Fprintf(w, "\nDeploy key not yet registered at %s.\n", hostnameFromURL(baseURL))
		if deployPubKey != "" {
			_, _ = fmt.Fprintf(w, "Register the upstream public key, then verify the connection:\n")
		} else {
			_, _ = fmt.Fprintf(w, "Retrieve the upstream public key, register it with your forge account, then verify the connection:\n")
		}
		_, _ = fmt.Fprintf(w, "\n    kubectl exec -n %s deploy/oberth -- oberth upstream add --yes --no-wait %s %s\n", cfg.Namespace, name, baseURL)
		return ErrOnboardingPartial
	}

	_, _ = fmt.Fprintf(deps.Output, "\nWaiting for Oberth to become ready...\n")
	if err := WaitForReady(ctx, cfg, deps); err != nil {
		return err
	}
	if deps.StepProgressSink != nil {
		deps.StepProgressSink("audit genesis", "done")
	}
	printReadyWithNextSteps(w, name, baseURL, sshConfigured, color)
	return nil
}

// promptUpstream asks for the upstream forge URL. An empty answer skips
// onboarding; up to three malformed answers are re-prompted. The prompt
// appears below the table's bottom border; after the user answers, the
// prompt is erased and the result is appended to the table.
func promptUpstream(ctx context.Context, cfg Config, deps Deps, tw *tableWriter) (baseURL, name string, ok bool, err error) {
	// Pre-filled path (TUI wizard): derive the upstream from config.
	if cfg.ForgeURL != "" {
		name, baseURL, parseErr := upstreamFromInput(cfg.ForgeURL)
		if parseErr != nil {
			return "", "", false, fmt.Errorf("invalid forge URL from config: %w", parseErr)
		}
		return baseURL, name, true, nil
	}

	w := deps.Output
	color := isColor(deps)

	for attempt := 0; attempt < 3; attempt++ {
		startPrompt(w, color, "Upstream", "github.com/your-org: ")
		raw, readErr := readLine(ctx, deps.Input)
		if readErr != nil {
			return "", "", false, fmt.Errorf("read upstream URL: %w", readErr)
		}
		raw = strings.TrimSpace(raw)
		if raw == "" {
			erasePromptLines(w, color, 1)
			_ = tw // table stays as-is on skip
			return "", "", false, nil
		}
		name, baseURL, parseErr := upstreamFromInput(raw)
		if parseErr != nil {
			completePrompt(w, color, "Upstream", parseErr.Error(), "✗ invalid")
			continue
		}
		// Erase the prompt line — the result will be appended to the table
		// by the caller after the upstream is registered.
		erasePromptLines(w, color, 1)
		return baseURL, name, true, nil
	}
	return "", "", false, nil
}

// upstreamFromInput turns "codeberg.org/acme" (or a full ssh:// URL)
// into an upstream name and ssh:// base URL for `oberth upstream add`.
func upstreamFromInput(raw string) (string, string, error) {
	base := raw
	if !strings.Contains(base, "://") {
		base = "ssh://git@" + base
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return "", "", fmt.Errorf("invalid upstream %q: %w", raw, err)
	}
	if parsed.Scheme != "ssh" {
		return "", "", fmt.Errorf("upstream must be host/organization or an ssh:// URL, got %q", raw)
	}
	if parsed.Hostname() == "" || strings.Trim(parsed.Path, "/") == "" {
		return "", "", errors.New("upstream must include a host and an organization, e.g. github.com/your-org")
	}
	if parsed.User == nil || parsed.User.Username() == "" {
		parsed.User = url.User("git")
	}
	name := strings.Split(parsed.Hostname(), ".")[0]
	return name, parsed.String(), nil
}

// promptDeployKey asks whether to generate the deploy key in-pod or provide
// an existing one. The prompt appears below the table; the result is appended
// to the table. Returns true when a key was provided (not generated).
//
// When the terminal supports raw mode, an inline arrow-key selector is shown
// (up/down/left/right to toggle, Enter to confirm, Ctrl+C to abort). When
// raw mode is unavailable (pipes, CI, tests), it falls back to the text-based
// [G]enerate / [P]rovide prompt.
func promptDeployKey(ctx context.Context, cfg Config, deps Deps, tw *tableWriter) (bool, error) {
	if cfg.UpstreamPrivateKeyPath != "" {
		path, err := expandSSHKeyPath(cfg.UpstreamPrivateKeyPath)
		if err != nil {
			return false, err
		}
		// #nosec G304 -- operator-selected work key path, imported over stdin.
		key, err := os.ReadFile(path)
		if err != nil {
			return false, fmt.Errorf("read selected work key: %w", err)
		}
		defer clear(key)
		if len(key) > 1<<20 {
			return false, errors.New("selected work key exceeds size limit")
		}
		if err := applyProvidedDeployKey(ctx, cfg, deps, key); err != nil {
			return false, err
		}
		tw.AppendRow("Upstream identity", path, "stored in OpenBao", false)
		return true, nil
	}
	// Non-interactive setup generates only when that option was selected.
	if !isInteractive(deps) {
		tw.AppendRow("Deploy key", "generated", "✓ stored", false)
		return false, nil
	}
	color := isColor(deps)
	for attempt := 0; ; attempt++ {
		idx, err := selectOption(ctx, deps, color, "Deploy key", []string{"Generate", "Provide"}, 1)
		if err != nil {
			return false, fmt.Errorf("read deploy-key choice: %w", err)
		}
		switch idx {
		case 0: // Generate
			// Erase the prompt, append the result to the table.
			erasePromptLines(deps.Output, color, 1)
			tw.AppendRow("Deploy key", "generated", "✓ stored", false)
			return false, nil
		case 1: // Provide
			erasePromptLines(deps.Output, color, 1)
			if err := provideDeployKey(ctx, cfg, deps, tw); err != nil {
				return false, err
			}
			return true, nil
		default: // invalid (-1) — text fallback only
			completePrompt(deps.Output, color, "Deploy key", "enter G or P", "✗ invalid")
			if attempt >= 2 {
				erasePromptLines(deps.Output, color, 1)
				tw.AppendRow("Deploy key", "skipped", "⚠ default", false)
				return false, nil
			}
		}
	}
}

// provideDeployKey reads an operator-supplied SSH PRIVATE key and stores it
// as the upstream deploy identity. When the terminal supports raw mode, an
// arrow-key file picker lists private keys found in ~/.ssh/ before falling
// back to the free-text prompt. The result is appended to the table.
func provideDeployKey(ctx context.Context, cfg Config, deps Deps, tw *tableWriter) error {
	w := deps.Output
	color := isColor(deps)

	var keyPath string

	// Interactive key picker when the terminal supports raw mode and color.
	if color && deps.MakeRaw != nil {
		if home, err := os.UserHomeDir(); err == nil {
			keys := listSSHKeys(filepath.Join(home, ".ssh"), false)
			if len(keys) > 0 {
				options := make([]string, len(keys)+1)
				copy(options, keys)
				options[len(keys)] = "Type path manually..."

				defaultIdx := 0
				for i, k := range keys {
					if k == "~/.ssh/id_ed25519" {
						defaultIdx = i
						break
					}
				}

				startPrompt(w, color, "Deploy key path", "")
				idx, selErr := selectFromList(deps, color, options, defaultIdx)
				if selErr != nil && !errors.Is(selErr, errRawModeUnavailable) {
					return fmt.Errorf("deploy key path selection: %w", selErr)
				}
				if selErr == nil && idx >= 0 && idx < len(keys) {
					keyPath = options[idx]
				}
				// "Type path manually..." or raw-mode failure -> keyPath stays
				// empty -> readLine. Erase the picker's prompt row so readLine
				// can reuse the line.
				if keyPath == "" {
					_, _ = fmt.Fprint(w, "\033[1A\r\033[2K")
				}
			}
		}
	}

	// Free-text fallback (or "Type path manually..." selected).
	if keyPath == "" {
		startPrompt(w, color, "Deploy key path", "~/.ssh/id_ed25519: ")
		path, err := readLine(ctx, deps.Input)
		if err != nil {
			return fmt.Errorf("read deploy-key path: %w", err)
		}
		keyPath = strings.TrimSpace(path)
		if keyPath == "" {
			keyPath = "~/.ssh/id_ed25519"
		}
	}

	expanded, err := expandSSHKeyPath(keyPath)
	if err != nil {
		erasePromptLines(w, color, 1)
		tw.AppendRow("Deploy key", keyPath, "✗ error", false)
		return err
	}
	// #nosec G304 -- the operator explicitly names their own key file.
	privateKey, err := os.ReadFile(expanded)
	if err != nil {
		erasePromptLines(w, color, 1)
		tw.AppendRow("Deploy key", keyPath, "✗ error", false)
		return fmt.Errorf("read deploy key: %w", err)
	}
	if err := applyProvidedDeployKey(ctx, cfg, deps, privateKey); err != nil {
		erasePromptLines(w, color, 1)
		tw.AppendRow("Deploy key", keyPath, "✗ error", false)
		return err
	}
	erasePromptLines(w, color, 1)
	tw.AppendRow("Deploy key", keyPath, "✓ stored", false)
	return nil
}

func applyProvidedDeployKey(ctx context.Context, cfg Config, deps Deps, privateKey []byte) error {
	_, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		var passphraseErr *ssh.PassphraseMissingError
		if errors.As(err, &passphraseErr) {
			return errors.New("the key is passphrase-protected; the pod needs a non-interactive key — generate a dedicated deploy key instead")
		}
		return fmt.Errorf("parse SSH private key: %w", err)
	}
	run := deps.RunCommand
	if run == nil {
		run = DefaultRunCommand
	}
	name, _, err := upstreamFromInput(cfg.ForgeURL)
	if err != nil {
		name = "default"
	}
	_, err = run(ctx, privateKey, "kubectl", kubectlOberthArgs(cfg, deps, true, "upstream", "provide-key", name)...)
	if err != nil {
		return fmt.Errorf("import existing work key into OpenBao: %w", err)
	}
	return nil
}

// registerUpstreamQuiet runs the pod's own `oberth upstream add --yes
// --no-wait` non-interactively, capturing output. --no-wait is load-bearing:
// without it the pod-side bootstrap blocks up to ProbeWait (10 minutes in
// production) probing a key the operator has not yet seen — the captured
// output means nothing reaches the terminal until the exec exits. With
// --no-wait a fresh generation prints the key and exits zero immediately via
// the ErrUpstreamBootstrapIncomplete path (which deliberately does NOT
// register the upstream row — a rerun after the operator registers the key
// completes it, which is exactly what the verification loop does).
//
// Returns whether the upstream is configured, the deploy public key extracted
// from the output, and any error. A publickey auth rejection (the key is not
// registered on the forge yet) is the one EXPECTED failure and returns
// (false, key, nil); every other failure — a knownhosts mismatch is an active
// MITM indicator, a handshake EOF means the endpoint is not an SSH forge — is
// returned as an error for the caller to surface. Silencing unknown errors
// here would reintroduce the fail-open classification issue #89 fixed in the
// pod-side wait loop.
func registerUpstreamQuiet(ctx context.Context, cfg Config, deps Deps, name, baseURL string) (bool, string, error) {
	run := deps.RunCommand
	if run == nil {
		run = DefaultRunCommand
	}
	addArgs := []string{"upstream", "add", "--yes", "--no-wait", name, baseURL}

	// The admin socket (/tmp/oberth-admin.sock) is bound just before the
	// server prints "ready for bootstrap". A pod that is Running but not
	// yet bootstrapped rejects exec commands with "connection refused".
	// The command is idempotent, so retrying is safe. We retry only when
	// the server produced no meaningful output (no public key) — once it
	// responds with a key, any remaining error is a real classification
	// (auth rejection, knownhosts mismatch) and must not be masked.
	var out []byte
	var err error
	var pubKey string
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return false, pubKey, ctx.Err()
			case <-time.After(pollInterval(deps)):
			}
		}
		out, err = run(ctx, nil, "kubectl", kubectlOberthArgs(cfg, deps, false, addArgs...)...)
		pubKey = extractDeployPublicKey(string(out))
		if err == nil {
			break
		}
		if pubKey != "" && isExpectedRegistrationPending(string(out)+" "+err.Error()) {
			// The bootstrap advertised the key and the forge rejected the
			// authentication — the normal "key not registered yet" state.
			return false, pubKey, nil
		}
		// Server responded with a key but a non-auth error (knownhosts
		// mismatch, handshake EOF) — a real failure, not a startup race.
		if pubKey != "" {
			break
		}
		// No key in the output: the admin socket likely is not ready yet.
	}
	if err != nil {
		return false, pubKey, fmt.Errorf("upstream add: %w%s", err, commandOutputSuffix(out))
	}
	configured, _ := upstreamConfigured(ctx, cfg, deps)
	return configured, pubKey, nil
}

// isExpectedRegistrationPending reports whether captured `upstream add`
// output describes the normal "deploy key not registered on the forge yet"
// publickey rejection. It mirrors the allowlist of the pod-side
// app.isExpectedAuthRejection (issue #89): matching is deliberately an
// ALLOWLIST of the x/crypto/ssh auth-failure signature, so a knownhosts
// mismatch, a handshake EOF, or any other unexpected failure classifies as
// NOT pending and gets surfaced by the caller. The contract is textual
// because it crosses the kubectl-exec boundary to the in-pod binary.
func isExpectedRegistrationPending(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "unable to authenticate") ||
		strings.Contains(lower, "no supported methods remain")
}

// terseInstallError compresses an error to one bounded line for warning
// output. Unlike the pod-side terseProbeError (which keeps the head, because
// a bare probe error leads with its leaf cause), this wraps captured exec
// output where Go error chains put the leaf cause LAST — so over-long text
// keeps the tail: truncating the head drops boilerplate, truncating the tail
// would drop the diagnosis (e.g. the knownhosts mismatch that indicates an
// active MITM).
func terseInstallError(err error) string {
	text := strings.Join(strings.Fields(err.Error()), " ")
	if runes := []rune(text); len(runes) > 160 {
		text = "..." + string(runes[len(runes)-157:])
	}
	return text
}

// extractDeployPublicKey finds the first SSH public key line in command output.
func extractDeployPublicKey(output string) string {
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "ssh-") {
			return trimmed
		}
	}
	return ""
}

// hostnameFromURL extracts the hostname from a URL string, falling back to the
// raw input when parsing fails.
func hostnameFromURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Hostname() == "" {
		return rawURL
	}
	return parsed.Hostname()
}

// onboardUplink registers the operator's first uplink. It returns the public
// key path, the identity, the bearer token (for deferred credential display),
// the raw sanitized uplink-add output (the fallback when token extraction
// fails — the token is shown exactly once and must never be silently
// discarded), and any error. Prompts appear below the table.
func onboardUplink(ctx context.Context, cfg Config, deps Deps, tw *tableWriter) (pubKeyPath, identity, token, rawOutput string, err error) {
	w := deps.Output
	color := isColor(deps)

	// --- SSH public key path ---
	if cfg.SSHPublicKeyPath != "" {
		// Pre-filled (TUI wizard): use the path directly, skip prompts.
		pubKeyPath = cfg.SSHPublicKeyPath
	} else {
		// Interactive: key picker or free-text prompt.
		if color && deps.MakeRaw != nil {
			if home, homeErr := os.UserHomeDir(); homeErr == nil {
				keys := listSSHKeys(filepath.Join(home, ".ssh"), true)
				if len(keys) > 0 {
					options := make([]string, len(keys)+1)
					copy(options, keys)
					options[len(keys)] = "Type path manually..."

					defaultIdx := 0
					for i, k := range keys {
						if k == defaultUplinkKeyPath {
							defaultIdx = i
							break
						}
					}

					startPrompt(w, color, "Uplink key", "")
					idx, selErr := selectFromList(deps, color, options, defaultIdx)
					if selErr != nil && !errors.Is(selErr, errRawModeUnavailable) {
						return "", "", "", "", fmt.Errorf("uplink key selection: %w", selErr)
					}
					if selErr == nil && idx >= 0 && idx < len(keys) {
						pubKeyPath = options[idx]
					}
					if pubKeyPath == "" {
						_, _ = fmt.Fprint(w, "\033[1A\r\033[2K")
					}
				}
			}
		}

		if pubKeyPath == "" {
			startPrompt(w, color, "Uplink key", defaultUplinkKeyPath+": ")
			rawPath, readErr := readLine(ctx, deps.Input)
			if readErr != nil {
				return "", "", "", "", fmt.Errorf("read public-key path: %w", readErr)
			}
			pubKeyPath = strings.TrimSpace(rawPath)
			if pubKeyPath == "" {
				pubKeyPath = defaultUplinkKeyPath
			}
		}
	}

	expanded, expandErr := expandSSHKeyPath(pubKeyPath)
	if expandErr != nil {
		if isInteractive(deps) {
			completePrompt(w, color, "Uplink key", pubKeyPath, "✗ error")
		}
		return "", "", "", "", expandErr
	}
	// #nosec G304 -- the operator explicitly names their own public-key file.
	publicKey, readFileErr := os.ReadFile(expanded)
	if readFileErr != nil {
		if isInteractive(deps) {
			completePrompt(w, color, "Uplink key", pubKeyPath, "✗ error")
		}
		return "", "", "", "", fmt.Errorf("read public key %s: %w", pubKeyPath, readFileErr)
	}
	if err := validatePublicKeyFile(publicKey); err != nil {
		if isInteractive(deps) {
			completePrompt(w, color, "Uplink key", pubKeyPath, "✗ error")
		}
		return "", "", "", "", err
	}
	// Erase the key-path prompt (no-op when non-interactive / no color).
	erasePromptLines(w, color, 1)
	tw.AppendRow("Uplink key", pubKeyPath, "✓ read", false)

	// --- Identity ---
	if cfg.UplinkIdentity != "" {
		// Pre-filled (TUI wizard): use the identity directly.
		identity = cfg.UplinkIdentity
	} else {
		// Interactive: prompt for identity.
		identityDefault := defaultUplinkIdentity()
		for attempt := 0; ; attempt++ {
			startPrompt(w, color, "Uplink", identityDefault+": ")
			raw, readErr := readLine(ctx, deps.Input)
			if readErr != nil {
				return "", "", "", "", fmt.Errorf("read uplink identity: %w", readErr)
			}
			identity = strings.TrimSpace(raw)
			if identity == "" {
				identity = identityDefault
			}
			if validErr := validateUplinkIdentity(identity); validErr != nil {
				completePrompt(w, color, "Uplink", validErr.Error(), "✗ invalid")
				if attempt >= 2 {
					return "", "", "", "", fmt.Errorf("invalid uplink identity after 3 attempts: %w", validErr)
				}
				continue
			}
			break
		}
		// Erase the identity prompt line.
		erasePromptLines(w, color, 1)
	}

	run := deps.RunCommand
	if run == nil {
		run = DefaultRunCommand
	}
	out, cmdErr := run(ctx, publicKey, "kubectl", kubectlOberthArgs(cfg, deps, true, "uplink", "add", "-", identity)...)
	if cmdErr != nil {
		return "", "", "", "", fmt.Errorf("uplink add: %w%s", cmdErr, commandOutputSuffix([]byte(sanitizeExecOutput(string(out)))))
	}
	token = extractUplinkToken(string(out))
	return expanded, identity, token, sanitizeExecOutput(string(out)), nil
}

// offerSSHConfig prompts the user to add an SSH config block, appending the
// result to the table. Returns ErrInterrupted when the user presses Ctrl+C
// during the prompt so the caller can exit cleanly.
func offerSSHConfig(ctx context.Context, deps Deps, pubKeyPath string, tw *tableWriter) error {
	w := deps.Output
	color := isColor(deps)
	host := sshHostFromServer(deps)
	keyPath := privateKeyPath(pubKeyPath)

	// Verify the derived private key exists.
	if _, err := os.Stat(keyPath); err != nil {
		tw.AppendRow("SSH config", "private key not found", "⚠ manual", false)
		return nil
	}
	if !isSSHConfigSafePath(keyPath) {
		tw.AppendRow("SSH config", "unsafe path", "⚠ manual", false)
		return nil
	}

	homeDir := deps.UserHomeDir
	if homeDir == nil {
		homeDir = os.UserHomeDir
	}
	home, err := homeDir()
	if err != nil {
		tw.AppendRow("SSH config", "no home dir", "⚠ skip", false)
		return nil
	}
	sshDir := filepath.Join(home, ".ssh")
	configPath := filepath.Join(sshDir, "config")

	// Check if the block already exists.
	if existingData, readErr := os.ReadFile(configPath); readErr == nil { //nolint:gosec // G304: path is derived from os.UserHomeDir
		if hasSSHHostBlock(existingData, "oberth") {
			// Verify the IdentityFile matches the uplink key — a previous
			// install may point at a different key than the one just registered.
			existingIdentity := extractSSHBlockIdentityFile(existingData, "oberth")
			if existingIdentity != "" {
				expandedExisting, expandErr := expandHomePath(existingIdentity)
				if expandErr != nil {
					expandedExisting = existingIdentity
				}
				if expandedExisting != keyPath {
					updated := updateSSHBlockIdentityFile(existingData, "oberth", keyPath)
					if writeErr := atomicWriteFile(configPath, updated, 0600); writeErr != nil {
						tw.AppendRow("SSH config", "~/.ssh/config", "✗ error", false)
						return nil
					}
					tw.AppendRow("SSH config", "~/.ssh/config", "✓ updated", false)
					return nil
				}
			}
			tw.AppendRow("SSH config", "~/.ssh/config", "✓ exists", false)
			return nil
		}
	}

	// Non-interactive (TUI wizard): auto-accept SSH config addition.
	// Interactive: prompt the operator.
	if isInteractive(deps) {
		startPrompt(w, color, "SSH config", "add to ~/.ssh/config? [Y/n]: ")
		answer, readErr := readLine(ctx, deps.Input)
		if readErr != nil {
			if errors.Is(readErr, ErrInterrupted) {
				return ErrInterrupted
			}
			erasePromptLines(w, color, 1)
			tw.AppendRow("SSH config", "~/.ssh/config", "⚠ skip", false)
			return nil
		}
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(answer)), "n") {
			erasePromptLines(w, color, 1)
			tw.AppendRow("SSH config", "~/.ssh/config", "skipped", false)
			return nil
		}
	}

	if err := os.MkdirAll(sshDir, 0700); err != nil {
		erasePromptLines(w, color, 1)
		tw.AppendRow("SSH config", "~/.ssh/config", "✗ error", false)
		return nil
	}

	block := GenerateSSHConfigBlock(host, sshNodePort, keyPath)
	// Read existing content (may be empty or nonexistent).
	existing, _ := os.ReadFile(configPath) //nolint:gosec // G304: path is derived from os.UserHomeDir
	newContent := append(existing, []byte("\n"+block)...)
	if writeErr := atomicWriteFile(configPath, newContent, 0600); writeErr != nil {
		erasePromptLines(w, color, 1)
		tw.AppendRow("SSH config", "~/.ssh/config", "✗ error", false)
		return nil
	}
	erasePromptLines(w, color, 1)
	tw.AppendRow("SSH config", "~/.ssh/config", "✓ added", false)
	return nil
}

// extractUplinkToken parses the token value from the uplink add command output.
// The output format is "Uplink token for ...(shown once):\n<token>\n".
func extractUplinkToken(output string) string {
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "Uplink token for ") && i+1 < len(lines) {
			return strings.TrimSpace(lines[i+1])
		}
	}
	// Fallback: if only one non-empty line that looks like a token
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "oberth_") {
			return trimmed
		}
	}
	return ""
}

// validatePublicKeyFile checks that the file content is an SSH public key (in
// authorized_keys format), not a private key. A common operator mistake is
// pointing the public-key prompt at id_ed25519 instead of id_ed25519.pub;
// without this check the private key is sent to the pod via kubectl exec stdin.
func validatePublicKeyFile(data []byte) error {
	// Check for private key material first — ssh.ParsePrivateKey accepts PEM
	// private keys, so if it succeeds this is a private key, not a public one.
	if _, err := ssh.ParsePrivateKey(data); err == nil {
		return errors.New("that is a PRIVATE key, not a public key — provide the .pub file instead (e.g. ~/.ssh/id_ed25519.pub)")
	}
	// Try parsing as an authorized_keys line (the expected format).
	if _, _, _, _, err := ssh.ParseAuthorizedKey(data); err != nil {
		return fmt.Errorf("file does not contain a valid SSH public key: %w", err)
	}
	return nil
}

// sanitizeExecOutput strips lines containing bearer tokens (oberth_*) from
// kubectl exec output before embedding it in error messages or non-credential
// display, so tokens are never leaked through error strings or log lines.
func sanitizeExecOutput(output string) string {
	var cleaned []string
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "oberth_") {
			continue
		}
		cleaned = append(cleaned, line)
	}
	return strings.Join(cleaned, "\n")
}

func defaultUplinkIdentity() string {
	user := os.Getenv("USER")
	if user == "" {
		user = "operator"
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "localhost"
	}
	return user + "@" + strings.Split(host, ".")[0]
}

// ---------------------------------------------------------------------------
// selectOption — inline arrow-key selector for two-or-more-option prompts
// ---------------------------------------------------------------------------

// selectOption presents an inline option selector within a borderless prompt
// line. When the terminal supports raw mode (MakeRaw != nil), arrow keys
// (up/down/left/right) toggle between options and Enter confirms. The
// selected option is indicated with a "▸" marker. Ctrl+C (byte 0x03)
// restores the terminal and returns ErrInterrupted.
//
// When raw mode is unavailable, it falls back to a text-based
// "[X]yz / [A]bc:" prompt parsed by readLine.
//
// All exit paths — success, error, Ctrl+C, and panic — restore the terminal
// state via deferred restore.
//
// Returns the selected index (0-based) and any error.
func selectOption(ctx context.Context, deps Deps, color bool, component string, options []string, defaultIdx int) (int, error) {
	if deps.MakeRaw == nil {
		return selectOptionText(ctx, deps, color, component, options, defaultIdx)
	}

	restore, err := deps.MakeRaw()
	if err != nil {
		// Raw mode failed (e.g. not a real terminal); fall back to text.
		return selectOptionText(ctx, deps, color, component, options, defaultIdx)
	}
	defer restore()

	selected := defaultIdx
	if selected < 0 || selected >= len(options) {
		selected = 0
	}

	// Render the initial selector state.
	writeSelectorLine(deps.Output, color, component, options, selected)

	buf := make([]byte, 3)
	for {
		n, readErr := deps.Input.Read(buf[:1])
		if readErr != nil {
			_, _ = fmt.Fprint(deps.Output, "\r\n")
			return -1, readErr
		}
		if n == 0 {
			continue
		}

		switch buf[0] {
		case 0x03: // Ctrl+C
			_, _ = fmt.Fprint(deps.Output, "\r\n")
			return -1, ErrInterrupted

		case '\r', '\n': // Enter — confirm selection
			_, _ = fmt.Fprint(deps.Output, "\r\n")
			return selected, nil

		case 0x1b: // Escape sequence start (arrow keys)
			// Read the next two bytes: expect '[' then direction.
			n1, _ := deps.Input.Read(buf[1:2])
			if n1 == 0 || buf[1] != '[' {
				continue
			}
			n2, _ := deps.Input.Read(buf[2:3])
			if n2 == 0 {
				continue
			}
			switch buf[2] {
			case 'A', 'D': // Up or Left — previous option
				if selected > 0 {
					selected--
					writeSelectorLine(deps.Output, color, component, options, selected)
				}
			case 'B', 'C': // Down or Right — next option
				if selected < len(options)-1 {
					selected++
					writeSelectorLine(deps.Output, color, component, options, selected)
				}
			}
		}
	}
}

// writeSelectorLine redraws the selector prompt inline using carriage return.
// Two-space indent, bold label, then options with a "▸" marker.
func writeSelectorLine(w io.Writer, color bool, component string, options []string, selected int) {
	bold, reset := "", ""
	if color {
		bold = "\033[1m"
		reset = "\033[0m"
	}

	var detail strings.Builder
	for i, opt := range options {
		if i > 0 {
			detail.WriteString("  ")
		}
		if i == selected {
			detail.WriteString("▸ ")
		} else {
			detail.WriteString("  ")
		}
		detail.WriteString(opt)
	}

	// Carriage return overwrites the current line; no newline follows so
	// subsequent redraws overwrite this one.
	_, _ = fmt.Fprintf(w, "\r  %s%s%s  %s", bold, component, reset, detail.String())
}

// selectOptionText is the non-raw-mode fallback for selectOption. It
// presents options as a text prompt (e.g. "[G]enerate / [P]rovide: ") and
// reads a line of input. Returns the matching index, defaultIdx on empty
// input, or -1 when the input does not match any option.
func selectOptionText(ctx context.Context, deps Deps, color bool, component string, options []string, defaultIdx int) (int, error) {
	var prompt strings.Builder
	for i, opt := range options {
		if i > 0 {
			prompt.WriteString(" / ")
		}
		if len(opt) > 0 {
			prompt.WriteByte('[')
			prompt.WriteByte(opt[0])
			prompt.WriteByte(']')
			prompt.WriteString(opt[1:])
		}
	}
	prompt.WriteString(": ")

	startPrompt(deps.Output, color, component, prompt.String())
	answer, err := readLine(ctx, deps.Input)
	if err != nil {
		return -1, err
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	if answer == "" {
		return defaultIdx, nil
	}
	for i, opt := range options {
		lower := strings.ToLower(opt)
		if answer == lower || (len(answer) == 1 && answer[0] == lower[0]) {
			return i, nil
		}
	}
	return -1, nil // invalid — caller decides retry or default
}

// ---------------------------------------------------------------------------
// selectFromList — vertical arrow-key file selector
// ---------------------------------------------------------------------------

// errRawModeUnavailable reports that selectFromList could not put the
// terminal into raw mode (MakeRaw is nil or failed). Callers treat it as
// "picker unavailable" and fall back to the free-text prompt.
var errRawModeUnavailable = errors.New("raw mode unavailable")

// selectFromList presents a vertical list of options below the current cursor
// position, indented with spaces and a "▸" marker for the selected item.
//
// It puts the terminal into raw mode for the duration of the selection and
// restores it on ALL exit paths — success, error, Ctrl+C, and panic — via
// deferred restore, the same lifecycle selectOption uses (the caller-managed
// sequential restore was the v0.13.5 pre-fix pattern; see issue #108). When
// raw mode cannot be entered it returns errRawModeUnavailable without
// touching the terminal.
//
// Arrow keys (up/down) navigate with wrapping; Enter confirms; Ctrl+C returns
// ErrInterrupted. After confirmation or cancellation, all option lines are
// erased, leaving the cursor one line below the prompt row.
func selectFromList(deps Deps, color bool, options []string, defaultIdx int) (int, error) {
	if len(options) == 0 {
		return -1, errors.New("no options to select from")
	}
	if deps.MakeRaw == nil {
		return -1, errRawModeUnavailable
	}
	restore, rawErr := deps.MakeRaw()
	if rawErr != nil {
		return -1, errRawModeUnavailable
	}
	defer restore()
	w, r := deps.Output, deps.Input
	if defaultIdx < 0 || defaultIdx >= len(options) {
		defaultIdx = 0
	}
	current := defaultIdx
	n := len(options)

	bold, reset := "", ""
	if color {
		bold = "\033[1m"
		reset = "\033[0m"
	}

	draw := func() {
		for i, opt := range options {
			_, _ = fmt.Fprint(w, "\r\033[2K")
			if i == current {
				_, _ = fmt.Fprintf(w, "    %s▸ %s%s", bold, opt, reset)
			} else {
				_, _ = fmt.Fprintf(w, "      %s", opt)
			}
			_, _ = fmt.Fprint(w, "\n")
		}
	}

	// Move below the prompt row and draw the initial option list.
	_, _ = fmt.Fprint(w, "\n")
	draw()

	buf := make([]byte, 1)
	for {
		if _, err := r.Read(buf); err != nil {
			return -1, err
		}
		switch buf[0] {
		case 0x03: // Ctrl+C
			_, _ = fmt.Fprintf(w, "\033[%dA\r\033[J", n)
			return -1, ErrInterrupted

		case '\r', '\n': // Enter — confirm selection
			_, _ = fmt.Fprintf(w, "\033[%dA\r\033[J", n)
			return current, nil

		case 0x1b: // ESC — start of arrow key sequence
			seq := make([]byte, 2)
			if _, err := io.ReadFull(r, seq); err != nil {
				continue
			}
			if seq[0] != '[' {
				continue
			}
			switch seq[1] {
			case 'A': // up
				current--
				if current < 0 {
					current = n - 1
				}
			case 'B': // down
				current++
				if current >= n {
					current = 0
				}
			default:
				continue
			}
			_, _ = fmt.Fprintf(w, "\033[%dA", n)
			draw()
		}
	}
}

// readLine reads one line WITHOUT buffering ahead: the same reader may later
// be piped verbatim into an interactive kubectl exec, so no lookahead may be
// consumed here.
//
// The ctx parameter allows cancellation (typically via SIGINT caught by
// signal.NotifyContext in main) to unblock a Read that would otherwise hang
// forever in cooked-mode terminals, where the kernel consumes Ctrl+C before
// the byte reaches userspace.
func readLine(ctx context.Context, reader io.Reader) (string, error) {
	if reader == nil {
		return "", errors.New("no input available")
	}
	type readResult struct {
		b   byte
		err error
	}
	// Unbuffered: the goroutine cannot proceed past its send until the
	// consumer receives, preventing it from entering the next Read and
	// consuming lookahead that belongs to a later kubectl-exec pipe.
	ch := make(chan readResult)
	// next gates each Read: the goroutine waits for permission before
	// calling reader.Read, so on return (newline, ETX, cancel) the
	// goroutine is blocked on <-next — not inside Read — and no byte
	// beyond the line is consumed. One persistent goroutine replaces the
	// per-byte goroutine spawn of the previous implementation.
	next := make(chan struct{}, 1)
	next <- struct{}{} // permit the first read
	go func() {
		buf := make([]byte, 1)
		<-next
		for {
			n, err := reader.Read(buf)
			if n > 0 {
				ch <- readResult{b: buf[0]}
			}
			if err != nil {
				ch <- readResult{err: err}
				return
			}
			if n > 0 {
				<-next // wait for permission before the next read
			}
			// n == 0 && err == nil: nothing happened, retry immediately
		}
	}()
	var line []byte
	for {
		select {
		case <-ctx.Done():
			return "", ErrInterrupted
		case r := <-ch:
			if r.err != nil {
				if errors.Is(r.err, io.EOF) && len(line) > 0 {
					return strings.TrimSuffix(string(line), "\r"), nil
				}
				if errors.Is(r.err, io.EOF) {
					return "", io.EOF
				}
				return "", r.err
			}
			if r.b == 0x03 { // ETX — Ctrl+C
				return "", ErrInterrupted
			}
			if r.b == '\n' {
				return strings.TrimSuffix(string(line), "\r"), nil
			}
			line = append(line, r.b)
			next <- struct{}{} // permit the next read
		}
	}
}

func expandHomePath(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/")), nil
	}
	return path, nil
}

// expandSSHKeyPath normalises an SSH key path: a pure filename (no path
// separator, not starting with ~) is treated as a file in ~/.ssh/. Anything
// containing a separator — including "./x", "../x", and "dir/key" — keeps
// its literal filesystem meaning, so the path the table displays is the path
// that is read: prefixing separator-containing input would silently reroute
// "../x" to ~/x while displaying "../x". After normalisation, tilde
// expansion is applied via expandHomePath.
func expandSSHKeyPath(path string) (string, error) {
	if path != "" && !strings.ContainsRune(path, '/') && !strings.HasPrefix(path, "~") {
		path = "~/.ssh/" + path
	}
	return expandHomePath(path)
}

const (
	// sshNodePort is the fixed SSH NodePort the Oberth chart exposes for
	// Git-over-SSH access. It is the chart default and matches the kind
	// cluster port mapping.
	sshNodePort = "30022"
)

// GenerateSSHConfigBlock produces the SSH config snippet for the oberth host.
// The IdentityFile value is always double-quoted to handle paths with spaces.
func GenerateSSHConfigBlock(host, port, identityFile string) string {
	return fmt.Sprintf("Host oberth\n    HostName %s\n    Port %s\n    User git\n    IdentityFile \"%s\"\n    IdentitiesOnly yes\n", host, port, identityFile)
}

// sshHostFromServer determines the SSH host to use based on the cluster
// configuration. For kind clusters, it is always localhost. For k3s/k0s, the
// node IP is parsed from the API server URL; loopback addresses are normalised
// to "localhost".
func sshHostFromServer(deps Deps) string {
	if deps.KindClusterName != "" {
		return "localhost"
	}
	if deps.RestConfig == nil || deps.RestConfig.Host == "" {
		return "localhost"
	}
	parsed, err := url.Parse(deps.RestConfig.Host)
	if err != nil {
		return "localhost"
	}
	host := parsed.Hostname()
	if host == "" {
		return "localhost"
	}
	if host == "localhost" {
		return "localhost"
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return "localhost"
	}
	return host
}

// privateKeyPath strips the .pub suffix from a public key path to derive the
// private key path.
func privateKeyPath(pubPath string) string {
	return strings.TrimSuffix(pubPath, ".pub")
}

// hasSSHHostBlock scans the content of an SSH config file for a "Host oberth"
// block. Matching is case-insensitive for the keyword (ssh_config treats
// "Host" and "host" equivalently) and case-sensitive for the host name.
func hasSSHHostBlock(data []byte, hostName string) bool {
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) <= 5 {
			continue
		}
		if strings.EqualFold(trimmed[:5], "Host ") && strings.TrimSpace(trimmed[5:]) == hostName {
			return true
		}
	}
	return false
}

// extractSSHBlockIdentityFile returns the IdentityFile value from the named
// Host block in an SSH config, or "" if not found. Quotes around the value are
// stripped; the keyword is matched case-insensitively (ssh_config convention).
func extractSSHBlockIdentityFile(data []byte, hostName string) string {
	lines := strings.Split(string(data), "\n")
	inBlock := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Host keyword starts a new block.
		if len(trimmed) > 5 && strings.EqualFold(trimmed[:5], "Host ") {
			if inBlock {
				return "" // hit next Host block without finding IdentityFile
			}
			if strings.TrimSpace(trimmed[5:]) == hostName {
				inBlock = true
			}
			continue
		}
		// Match keyword also starts a new block.
		if len(trimmed) > 6 && strings.EqualFold(trimmed[:6], "Match ") {
			if inBlock {
				return ""
			}
			continue
		}
		if !inBlock {
			continue
		}
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "identityfile") {
			rest := strings.TrimSpace(trimmed[len("identityfile"):])
			if len(rest) >= 2 && rest[0] == '"' && rest[len(rest)-1] == '"' {
				rest = rest[1 : len(rest)-1]
			}
			return rest
		}
	}
	return ""
}

// updateSSHBlockIdentityFile replaces the IdentityFile line within the named
// Host block. The replacement preserves leading whitespace. If no IdentityFile
// line is found the original data is returned unchanged.
func updateSSHBlockIdentityFile(data []byte, hostName, newKeyPath string) []byte {
	lines := strings.Split(string(data), "\n")
	inBlock := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) > 5 && strings.EqualFold(trimmed[:5], "Host ") {
			if strings.TrimSpace(trimmed[5:]) == hostName {
				inBlock = true
			} else {
				inBlock = false
			}
			continue
		}
		if len(trimmed) > 6 && strings.EqualFold(trimmed[:6], "Match ") {
			inBlock = false
			continue
		}
		if !inBlock {
			continue
		}
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "identityfile") {
			indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
			lines[i] = fmt.Sprintf("%sIdentityFile \"%s\"", indent, newKeyPath)
			return []byte(strings.Join(lines, "\n"))
		}
	}
	return data
}

// isSSHConfigSafePath reports whether a file path is safe to embed in an
// ssh_config IdentityFile directive (double-quoted). Paths containing double
// quotes or ASCII control characters would corrupt the config file.
func isSSHConfigSafePath(path string) bool {
	for _, r := range path {
		if r == '"' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// validateUplinkIdentity checks that identity is in the required name@host
// format: exactly one "@", both parts non-empty, no whitespace.
func validateUplinkIdentity(identity string) error {
	if strings.ContainsAny(identity, " \t") {
		return fmt.Errorf("identity must be in the form name@host (e.g., alice@laptop)")
	}
	if strings.Count(identity, "@") != 1 {
		return fmt.Errorf("identity must be in the form name@host (e.g., alice@laptop)")
	}
	parts := strings.SplitN(identity, "@", 2)
	if parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("identity must be in the form name@host (e.g., alice@laptop)")
	}
	return nil
}

// sshHostBlockExists reports whether ~/.ssh/config already contains a
// "Host oberth" block, used to decide the clone URL format in next-steps.
func sshHostBlockExists() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	data, err := os.ReadFile(filepath.Join(home, ".ssh", "config")) //nolint:gosec // G304: path is derived from os.UserHomeDir
	if err != nil {
		return false
	}
	return hasSSHHostBlock(data, "oberth")
}

// printReadyWithNextSteps prints the Ready banner followed by a table of
// next-step commands using the operator's configured SSH alias.
func printReadyWithNextSteps(w io.Writer, upstream, baseURL string, sshConfigured, color bool) {
	if color {
		_, _ = fmt.Fprintf(w, "\n\033[1;32mReady\033[0m — %s\n", oberthWebUIURL)
	} else {
		_, _ = fmt.Fprintf(w, "\nReady — %s\n", oberthWebUIURL)
	}

	org := orgFromBaseURL(baseURL)
	repoRef := upstream + "/<org>/<repo>"
	if org != "" {
		repoRef = upstream + "/" + org + "/<repo>"
	}

	var cloneCmd string
	if sshConfigured {
		cloneCmd = fmt.Sprintf("git clone git@oberth:%s.git", repoRef)
	} else {
		cloneCmd = fmt.Sprintf("git clone ssh://git@localhost:%s/%s.git", sshNodePort, repoRef)
	}

	_, _ = fmt.Fprintf(w, "\n  %-46s %s\n", "Command", "Description")
	_, _ = fmt.Fprintf(w, "  %-46s %s\n", "──────────────────────────────────────────────", "───────────────────────────────")
	_, _ = fmt.Fprintf(w, "  %-46s %s\n", cloneCmd, "Clone any repo through Oberth")
	_, _ = fmt.Fprintf(w, "  %-46s %s\n", "cd <repo> && oberth init", "Generate a pipeline")
	_, _ = fmt.Fprintf(w, "  %-46s %s\n", "git push -u origin my-branch", "Push triggers CI")
	_, _ = fmt.Fprintln(w)
}

// orgFromBaseURL extracts the organization (last path segment) from an
// upstream base URL like "ssh://git@github.com/valariantech". Returns "" when
// the URL is empty or unparseable.
//
// The canonical org derivation is model.Upstream.Org(); this function must
// agree with it for URL-type inputs that pass upstreamFromInput validation
// (which guarantees a host and a non-empty path). The two use different
// parsing strategies (url.Parse vs string split), but produce identical
// results for validated inputs.
func orgFromBaseURL(baseURL string) string {
	if baseURL == "" {
		return ""
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	path := strings.Trim(parsed.Path, "/")
	if path == "" {
		return ""
	}
	parts := strings.Split(path, "/")
	return parts[len(parts)-1]
}

// atomicWriteFile writes data to a file atomically by first writing to a
// temporary file in the same directory and then renaming it to the target
// path. This prevents a crash window where the target file is truncated but
// not yet fully written — the rename is atomic on POSIX filesystems, so the
// target is either the old content or the complete new content.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".oberth-config-*")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err = file.Chmod(perm); err == nil {
		_, err = file.Write(data)
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp, path)
}

// ---------------------------------------------------------------------------
// SSH key scanning
// ---------------------------------------------------------------------------

// sshKeySkipNames is the set of filenames in ~/.ssh/ that are never SSH keys.
var sshKeySkipNames = map[string]bool{
	"known_hosts":      true,
	"known_hosts.old":  true,
	"config":           true,
	"authorized_keys":  true,
	"authorized_keys2": true,
	"environment":      true,
}

// listSSHKeys scans dir for SSH key files and returns their paths as ~/…
// strings, sorted alphabetically. When wantPub is true only .pub files are
// returned; when false only private keys (non-.pub, non-metadata) are
// returned. Each candidate is validated by reading the first line: private
// keys must start with "-----BEGIN", public keys with a recognised key type
// prefix (ssh-ed25519, ssh-rsa, ecdsa-sha2, ssh-dss, sk-ssh-). Returns nil
// when the directory is unreadable or contains no matching keys.
func listSSHKeys(dir string, wantPub bool) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var keys []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if sshKeySkipNames[name] {
			continue
		}

		isPub := strings.HasSuffix(name, ".pub")
		if wantPub && !isPub {
			continue
		}
		if !wantPub && isPub {
			continue
		}
		// SSH certificate files (*-cert.pub) are not key candidates.
		if wantPub && strings.HasSuffix(name, "-cert.pub") {
			continue
		}

		fullPath := filepath.Join(dir, name)
		if !isSSHKeyFile(fullPath, wantPub) {
			continue
		}

		// Present as ~/.ssh/<name> for display and later expansion.
		keys = append(keys, "~/.ssh/"+name)
	}
	sort.Strings(keys)
	return keys
}

// isSSHKeyFile checks whether the file at path looks like an SSH key by
// reading the first line. For private keys (wantPub=false) the line must
// start with "-----BEGIN". For public keys (wantPub=true) it must start with
// a recognised key type prefix.
func isSSHKeyFile(path string, wantPub bool) bool {
	info, statErr := os.Lstat(path)
	if statErr != nil {
		return false
	}
	// Skip symlinks, directories, and oversized files (>16 KB is not a key).
	if info.Mode()&os.ModeSymlink != 0 || info.IsDir() || info.Size() > 16384 {
		return false
	}

	// Read only the first 256 bytes — enough for the header line.
	// #nosec G304 -- path is derived from os.ReadDir of ~/.ssh/.
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, 256)
	n, err := f.Read(buf)
	if n == 0 || (err != nil && !errors.Is(err, io.EOF)) {
		return false
	}
	firstLine := strings.SplitN(string(buf[:n]), "\n", 2)[0]
	firstLine = strings.TrimSpace(firstLine)

	if wantPub {
		return strings.HasPrefix(firstLine, "ssh-ed25519") ||
			strings.HasPrefix(firstLine, "ssh-rsa") ||
			strings.HasPrefix(firstLine, "ecdsa-sha2-") ||
			strings.HasPrefix(firstLine, "ssh-dss") ||
			strings.HasPrefix(firstLine, "sk-ssh-") ||
			strings.HasPrefix(firstLine, "sk-ecdsa-")
	}
	return strings.HasPrefix(firstLine, "-----BEGIN")
}
