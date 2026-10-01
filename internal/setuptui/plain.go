package setuptui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/oberthci/oberth/internal/installer"
)

// runPlain implements the accessible/plain sequential fallback (S12).
// Same pages, same order, same validation, same security properties.
// No TUI, no color, no cursor addressing. It is reached only through the
// --plain / --accessible flags (or a non-interactive terminal) — never by a
// key press inside the TUI.
func runPlain(ctx context.Context, opts Options, input io.Reader, output io.Writer) error {
	w := func(format string, a ...any) { _, _ = fmt.Fprintf(output, format, a...) }
	wln := func(a ...any) { _, _ = fmt.Fprintln(output, a...) }
	// step prints the same "step n/N — stage" header the TUI's top bar
	// shows, with N derived from the page table so the two never drift.
	step := func(n int, stage string) { w("  step %d/%d — %s\n", n, totalPages, stage) }

	reader := bufio.NewReader(input)
	state := &WizardState{
		Config: installer.Config{
			Dev:       true,
			Namespace: "oberth",
		},
		StoreMode: "install-prod",
		ForgeType: "codeberg",
		ForgeAuth: "deploy-key",
		TLSMode:   "self-signed",
	}

	// ask prompts until validate accepts the answer (S12: the sequential
	// path keeps the same field-level validation the TUI enforces). An
	// empty answer selects def. Three rejected answers abort the wizard —
	// the same register as the installer's own prompts.
	ask := func(label, def string, validate func(string) error) (string, error) {
		for attempt := 0; attempt < 3; attempt++ {
			if def != "" {
				w("  %s [%s]: ", label, def)
			} else {
				w("  %s: ", label)
			}
			line, err := readLine(ctx, reader)
			if errors.Is(err, installer.ErrInterrupted) {
				return "", err // ctrl+c: stop here, main exits 130
			}
			if err != nil && line == "" {
				return "", fmt.Errorf("read %s: %w", label, err)
			}
			answer := strings.TrimSpace(line)
			if answer == "" {
				answer = def
			}
			if validate == nil {
				return answer, nil
			}
			if err := validate(answer); err != nil {
				wln("  ERROR: " + err.Error())
				continue
			}
			return answer, nil
		}
		return "", fmt.Errorf("%s: three invalid answers", label)
	}

	validNamespace := func(v string) error {
		if !dns1123LabelRegexp.MatchString(v) {
			return fmt.Errorf("%q: %s", v, namespaceRule)
		}
		return nil
	}

	// Page 1: Welcome.
	wln("")
	wln("  OBERTH SETUP")
	wln("  Machine-speed code. Human-grade control.")
	wln("")
	step(1, "mission briefing")
	wln("")

	// Page 2: Cluster. The installer targets the CURRENT kubeconfig
	// context — there is no --context flag — so naming a different context
	// here must stop the wizard rather than silently install into whatever
	// context happens to be current (wrong-cluster hazard).
	step(2, "launch site")
	currentContext := ""
	if raw, err := clientcmd.NewDefaultClientConfigLoadingRules().Load(); err == nil {
		currentContext = raw.CurrentContext
	}
	ctxAnswer, err := ask("Kubeconfig context (empty for current)", currentContext, func(v string) error {
		if v != "" && currentContext != "" && v != currentContext {
			return fmt.Errorf("installing into a non-current context is not supported yet — "+
				"run `kubectl config use-context %s` first, then re-run setup", v)
		}
		return nil
	})
	if err != nil {
		return err
	}
	state.SelectedContext = ctxAnswer

	// Page 3: Namespaces — DNS-1123 validated and pairwise distinct, the
	// same rules the TUI page enforces. (There is no mode page: the wizard
	// installs the dev / evaluation profile, the only one that exists.)
	step(3, "flight plan")
	ns, err := ask("Oberth namespace", "oberth", validNamespace)
	if err != nil {
		return err
	}
	state.Config.Namespace = ns
	// Default must match installer.DefaultArgoNamespace ("oberth-argo").
	argoNS, err := ask("Pipeline namespace", "oberth-argo", func(v string) error {
		if err := validNamespace(v); err != nil {
			return err
		}
		if v == ns {
			return fmt.Errorf("pipeline namespace must differ from the oberth namespace")
		}
		return nil
	})
	if err != nil {
		return err
	}
	state.Config.ArgoNamespace = argoNS
	baoNS, err := ask("OpenBao namespace", "openbao", func(v string) error {
		if err := validNamespace(v); err != nil {
			return err
		}
		if v == ns || v == argoNS {
			return fmt.Errorf("openbao namespace must differ from the other namespaces")
		}
		return nil
	})
	if err != nil {
		return err
	}
	state.Config.OpenBaoNamespace = baoNS

	// Page 4: Execution.
	step(4, "flight plan")
	np, err := ask("Network policy (auto/strict/off)", "auto", func(v string) error {
		if _, ok := canonicalNetworkPolicy(v); !ok {
			return fmt.Errorf("network policy must be auto, strict, or off")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if canonical, ok := canonicalNetworkPolicy(np); ok {
		state.Config.NetworkPolicy = canonical
	}
	anchor, err := ask("External anchoring (on/off)", "off", func(v string) error {
		if v != "on" && v != "off" {
			return fmt.Errorf("answer on or off")
		}
		return nil
	})
	if err != nil {
		return err
	}
	state.Config.InstallRekor = anchor == "on"

	// Page 5: Secret store.
	step(5, "propellant")
	wln("  [1] Install OpenBao — dev")
	wln("  [2] Install OpenBao — production")
	wln("  [3] Connect existing")
	choice, err := ask("Choice", "2", func(v string) error {
		if v != "1" && v != "2" && v != "3" {
			return fmt.Errorf("answer 1, 2, or 3")
		}
		return nil
	})
	if err != nil {
		return err
	}
	switch choice {
	case "1":
		state.Config.InstallSecretStoreDev = true
		state.StoreMode = "install-dev"
	case "3":
		state.StoreMode = "connect"
		// Page 6: Store connect — same S7 validator as the TUI field.
		step(6, "propellant")
		addr, err := ask("Vault address (https://…)", "", func(v string) error {
			return validateStoreAddress(v)
		})
		if err != nil {
			return err
		}
		state.Config.ArgoVaultAddress = addr
		state.StoreAddress = addr
	default:
		state.Config.InstallSecretStore = true
		state.StoreMode = "install-prod"
	}

	// Page 7: TLS — self-signed is the only implemented mode (no --tls-cert/
	// --tls-key in the installer). Informational only, like the TUI page.
	step(7, "heat shield")
	wln("  TLS: self-signed certificate (ed25519) will be generated.")
	wln("  Bring-your-own certificate support is coming soon.")
	state.TLSMode = "self-signed"

	// Page 8: Uplink.
	step(8, "crew manifest")
	user := os.Getenv("USER")
	if user == "" {
		user = "admin"
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "localhost"
	}
	identity, err := ask("Identity", user+"@"+host, func(v string) error {
		return validateUplinkIdentityTUI(v)
	})
	if err != nil {
		return err
	}
	state.UplinkIdentity = identity

	// SSH public key path — required by hasOnboardingConfig() for the
	// non-interactive onboarding path. Scan ~/.ssh for .pub files like the
	// TUI does; default to the first found key, falling back to
	// ~/.ssh/id_ed25519.pub.
	sshKeyDefault := "~/.ssh/id_ed25519.pub"
	if foundKeys := scanSSHPublicKeys(); len(foundKeys) > 0 {
		sshKeyDefault = foundKeys[0].path
	} else {
		wln("  No SSH public keys found in ~/.ssh.")
		wln("  Generate one:  ssh-keygen -t ed25519")
		wln("")
	}
	sshKey, err := ask("SSH public key", sshKeyDefault, func(v string) error {
		expanded := v
		if v == "~" || strings.HasPrefix(v, "~/") {
			home, homeErr := os.UserHomeDir()
			if homeErr != nil {
				return fmt.Errorf("resolve home directory: %w", homeErr)
			}
			expanded = home + v[1:]
		}
		data, readErr := os.ReadFile(expanded) //nolint:gosec // G304: operator names their own key
		if readErr != nil {
			return fmt.Errorf("cannot read %s: %w", v, readErr)
		}
		if _, _, _, _, parseErr := ssh.ParseAuthorizedKey(data); parseErr != nil {
			return fmt.Errorf("not a valid SSH public key: %w", parseErr)
		}
		return nil
	})
	if err != nil {
		return err
	}
	state.SSHKeyPath = sshKey

	// Page 9: Git (informational).
	step(9, "comms check")
	wln("  Push to Oberth over SSH — every push is linked to your identity.")
	wln("  clone: ssh://git@localhost:30022/<repo>.git   (from this machine)")
	wln("         ssh://git@<node-ip>:30022/<repo>.git   (from your network)")
	wln("  push → CI runs → green publishes upstream · red opens an issue")
	wln("  Tags are immutable — only green branches reach the upstream forge.")

	// Page 10: Forge.
	step(10, "ground station")
	forge, err := ask("Forge (codeberg/github/gitlab)", "codeberg", func(v string) error {
		switch v {
		case "codeberg", "github", "gitlab":
			return nil
		case "forgejo":
			return fmt.Errorf("forgejo support is coming soon — choose codeberg, github, or gitlab")
		}
		return fmt.Errorf("forge must be codeberg, github, or gitlab")
	})
	if err != nil {
		return err
	}
	state.ForgeType = forge
	org, err := ask("Owner/org", "", func(v string) error {
		if v == "" {
			return fmt.Errorf("org is required")
		}
		return nil
	})
	if err != nil {
		return err
	}
	state.ForgeOrg = org

	// Page 11: Review.
	step(11, "go/no-go")
	wln("  Review the plan:")
	wln("")
	w("  cluster:    %s\n", state.SelectedContext)
	w("  namespaces: %s\n", formatNamespacesSummary(state))
	w("  store:      %s\n", formatStoreSummary(state))
	w("  tls:        %s\n", state.TLSMode)
	w("  uplink:     %s\n", state.UplinkIdentity)
	w("  forge:      %s %s\n", state.ForgeType, state.ForgeOrg)
	wln("")

	if opts.DryMode {
		wln("  --dry-mode: equivalent command:")
		wln("")
		wln("  " + BuildCommandLine(state))
		return nil
	}

	confirm, err := ask("Apply? (yes/no)", "yes", func(v string) error {
		if v != "yes" && v != "y" && v != "no" && v != "n" {
			return fmt.Errorf("answer yes or no")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if confirm != "yes" && confirm != "y" {
		wln("  Aborted — nothing applied; re-run `oberth setup --plain` to try again.")
		return nil
	}

	// Page 12: Apply.
	step(12, "ignition")
	wln("  Applying...")

	state.Config.BinaryVersion = opts.BinaryVersion
	if state.Config.BinaryVersion == "" {
		state.Config.BinaryVersion = "dev"
	}
	// The wizard has resolved the secret store choice — never re-prompt.
	// When the user picked "connect existing", both InstallSecretStore flags
	// are false, but the decision is made: SecretStoreUndecided must be false.
	state.Config.SecretStoreUndecided = false

	// Wire the wizard-collected onboarding data into Config so the
	// installer's non-interactive onboarding path uses it instead of
	// re-prompting (which would EOF or get stuck). This is the same
	// wiring that the TUI's startApply performs (page_apply.go).
	state.Config.ForgeType = state.ForgeType
	state.Config.ForgeOrg = state.ForgeOrg
	state.Config.ForgeURL = forgeUpstreamURL(state.ForgeType, state.ForgeOrg)
	state.Config.UplinkIdentity = state.UplinkIdentity
	state.Config.SSHPublicKeyPath = state.SSHKeyPath
	state.Config.Yes = true

	return installer.Execute(ctx, state.Config, installer.InstallDeps{
		Output:     output,
		Input:      input,
		IsTerminal: func() bool { return false },
	})
}

// lineResult carries one line read off the prompt reader.
type lineResult struct {
	line string
	err  error
}

// readLine reads one line from r and gives up the moment ctx is canceled.
// main wires SIGINT into ctx (signal.NotifyContext), so ctrl+c at a prompt
// does not terminate the process by itself — a read that ignored ctx would
// sit there until the next enter, with every further ctrl+c swallowed too.
// Cancellation surfaces as installer.ErrInterrupted so main exits 130 the
// way every other interrupted prompt does. The reading goroutine may stay
// blocked on the terminal after cancellation; the process is on its way out
// and nothing reads that stream afterwards.
func readLine(ctx context.Context, r *bufio.Reader) (string, error) {
	ch := make(chan lineResult, 1)
	go func() {
		line, err := r.ReadString('\n')
		ch <- lineResult{line: line, err: err}
	}()
	select {
	case <-ctx.Done():
		return "", installer.ErrInterrupted
	case res := <-ch:
		return res.line, res.err
	}
}
