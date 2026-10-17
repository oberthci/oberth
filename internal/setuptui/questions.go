package setuptui

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"golang.org/x/crypto/ssh"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/mdnsdial"
	"github.com/oberthci/oberth/internal/secretstore"
)

// option is one choice on a choice screen.
type option struct {
	value  string
	label  string
	detail string
}

// probeResult is the outcome of a slow check (connecting to the cluster).
// apply runs on the UI goroutine; the probe itself never touches the state.
type probeResult struct {
	apply func(*WizardState)
	err   error
}

// question is one screen: a heading, a short explanation, and exactly one
// input — a choice list or a set of text fields. The TUI and the plain
// prompts render the same table, so both modes ask the same questions in the
// same order and validate them with the same code (#047, #011).
type question struct {
	id      string
	step    stepID
	heading string
	explain func(st *WizardState) string
	applies func(st *WizardState) bool

	// A choice screen.
	options  func(st *WizardState) []option
	selected func(st *WizardState) string

	// A text screen.
	fields       []string
	placeholders []string
	values       func(st *WizardState) []string

	// save validates the answer — the selected option's value, or one value
	// per field — and stores it. An error keeps the user on the screen.
	save func(st *WizardState, answer []string) error
	// probe optionally runs a slow check after save succeeded. It is called
	// on the UI goroutine and returns the work to run off it.
	probe func(st *WizardState) func() probeResult
}

func (q *question) isChoice() bool { return q.options != nil }

func (q *question) appliesTo(st *WizardState) bool { return q.applies == nil || q.applies(st) }

// answerOf is the stored answer, used to re-validate saved answers.
func (q *question) answerOf(st *WizardState) []string {
	if q.isChoice() {
		return []string{q.selected(st)}
	}
	return q.values(st)
}

// ---------- host probes, replaceable in tests ----------

// kindLookPath resolves the binaries kind creation needs; tests replace it.
var kindLookPath = exec.LookPath

// kindAvailable reports whether this host can create a kind cluster: the
// installer drives both the kind and docker CLIs. Without them the choice
// can only fail, so it is not offered (a k3s host on Linux, for example).
func kindAvailable() bool {
	for _, tool := range []string{"kind", "docker"} {
		if _, err := kindLookPath(tool); err != nil {
			return false
		}
	}
	return true
}

// noClusterAdvice is shown when no Kubernetes cluster is configured.
func noClusterAdvice() string {
	if kindAvailable() {
		return "Provide a kubeconfig path for an existing cluster, or create a new one with kind."
	}
	return "Provide a kubeconfig path for an existing cluster (install kind and Docker to create a local one)."
}

// clusterProbe connects to a cluster; tests replace it.
var clusterProbe = func(contextName, path string) clusterInfoMsg {
	msg, _ := probeClusterWithPath(contextName, path)().(clusterInfoMsg)
	return msg
}

// kubeContext represents one entry from a kubeconfig.
type kubeContext struct {
	name    string
	server  string
	isLocal bool
}

// loadKubeContexts lists a kubeconfig's contexts, the current one first. An
// empty path reads the default kubeconfig (KUBECONFIG or ~/.kube/config).
func loadKubeContexts(path string) ([]kubeContext, string, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = path
	raw, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).RawConfig()
	if err != nil {
		return nil, "", err
	}
	contexts := make([]kubeContext, 0, len(raw.Contexts))
	for name, ctx := range raw.Contexts {
		server := ""
		if cluster, ok := raw.Clusters[ctx.Cluster]; ok {
			server = cluster.Server
		}
		contexts = append(contexts, kubeContext{name: name, server: server, isLocal: isLocalServer(server)})
	}
	current := raw.CurrentContext
	sort.Slice(contexts, func(i, j int) bool {
		if (contexts[i].name == current) != (contexts[j].name == current) {
			return contexts[i].name == current
		}
		return contexts[i].name < contexts[j].name
	})
	return contexts, current, nil
}

// isLocalServer heuristically determines if a server URL is local.
func isLocalServer(server string) bool {
	for _, prefix := range []string{
		"https://127.", "https://localhost", "https://192.168.",
		"https://10.", "https://172.16.", "https://172.17.",
		"https://172.18.", "https://172.19.", "https://172.2",
		"https://172.3",
	} {
		if strings.HasPrefix(server, prefix) {
			return true
		}
	}
	return false
}

func contextOptions(contexts []kubeContext, current string) []option {
	opts := make([]option, 0, len(contexts))
	for _, c := range contexts {
		where := "remote"
		if c.isLocal {
			where = "local"
		}
		detail := where
		if c.server != "" {
			detail += " · " + c.server
		}
		if c.name == current {
			detail = "current · " + detail
		}
		opts = append(opts, option{value: c.name, label: c.name, detail: detail})
	}
	return opts
}

func probeContext(contextName, path string) func() probeResult {
	return func() probeResult {
		info := clusterProbe(contextName, path)
		if info.err != nil {
			return probeResult{err: info.err}
		}
		return probeResult{apply: func(st *WizardState) { st.ClusterInfo = info }}
	}
}

// ---------- validation shared by both modes ----------

// dns1123LabelRegexp validates a DNS-1123 label (a namespace).
var dns1123LabelRegexp = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// namespaceRule is the human reading of dns1123LabelRegexp.
const namespaceRule = "lowercase letters, digits and dashes only — start and end with a letter or digit"

// dnsNameRegexp validates a DNS name a certificate can carry.
var dnsNameRegexp = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// splitNames splits a comma- or space-separated list.
func splitNames(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
}

// validateForgeOrg applies the rule installer.Config.Validate enforces.
func validateForgeOrg(org string) error {
	if strings.TrimSpace(org) == "" {
		return errors.New("organization is required")
	}
	if strings.ContainsAny(org, "/\\?#@ \t\r\n") || org == "." || org == ".." {
		return errors.New("an organization is one name, without / ? # @ or spaces")
	}
	return nil
}

// suggestedIdentity builds name@host from $USER and the hostname, each
// reduced to characters an identity can carry, so the default always passes
// validation (#006: USER=dr@valarian.com used to produce two @ signs).
func suggestedIdentity() string {
	clean := func(s string) string {
		var b strings.Builder
		for _, r := range s {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
				b.WriteRune(r)
			default:
				b.WriteByte('-')
			}
		}
		return strings.Trim(b.String(), "-.")
	}
	user := clean(os.Getenv("USER"))
	if user == "" {
		user = "admin"
	}
	host, _ := os.Hostname()
	host, _, _ = strings.Cut(host, ".")
	host = clean(host)
	if host == "" {
		host = "localhost"
	}
	return user + "@" + host
}

func expandedPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("a path is required")
	}
	return expandSetupPath(path)
}

// checkPublicKeyFile reads and parses an SSH public key (S9: never a
// private key) and returns its fingerprint.
func checkPublicKeyFile(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the operator names their own public key
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %w", displayHome(path), err)
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey(data)
	if err != nil {
		return "", fmt.Errorf("%s is not an SSH public key", displayHome(path))
	}
	return ssh.FingerprintSHA256(key), nil
}

type sshKey struct {
	path        string
	algorithm   string
	fingerprint string
}

// scanSSHPublicKeys lists the parseable *.pub files in ~/.ssh (S9: private
// keys are never read).
func scanSSHPublicKeys() []sshKey {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	dir := filepath.Join(home, ".ssh")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var keys []sshKey
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".pub") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path) //nolint:gosec // G304: a listing of ~/.ssh, public keys only
		if err != nil {
			continue
		}
		pk, _, _, _, err := ssh.ParseAuthorizedKey(data)
		if err != nil {
			continue
		}
		keys = append(keys, sshKey{path: path, algorithm: strings.TrimPrefix(pk.Type(), "ssh-"), fingerprint: ssh.FingerprintSHA256(pk)})
	}
	return keys
}

// sshKeyPair is a private key in ~/.ssh found through its public half.
type sshKeyPair struct {
	sshKey
	private string
	comment string
}

// scanSSHKeyPairs lists the private keys in ~/.ssh that have a parseable
// .pub beside them (#057). Only the public halves are read; the private key
// is checked when it is chosen.
func scanSSHKeyPairs() []sshKeyPair {
	var pairs []sshKeyPair
	for _, k := range scanSSHPublicKeys() {
		private := strings.TrimSuffix(k.path, ".pub")
		if info, err := os.Stat(private); err != nil || !info.Mode().IsRegular() {
			continue
		}
		comment := ""
		if data, err := os.ReadFile(k.path); err == nil { //nolint:gosec // G304: a public key listed above
			if _, c, _, _, perr := ssh.ParseAuthorizedKey(data); perr == nil {
				comment = c
			}
		}
		pairs = append(pairs, sshKeyPair{sshKey: k, private: private, comment: comment})
	}
	return pairs
}

// errRescan asks a choice screen to list its options again, after the
// operator created a key in another terminal.
var errRescan = errors.New("rescan")

// normalizeAlsoUpstream turns "codeberg.org/acme" or an ssh:// URL into the
// ssh://git@host/org form the installer registers.
func normalizeAlsoUpstream(raw string) (string, error) {
	base := strings.TrimSpace(raw)
	if !strings.Contains(base, "://") {
		base = "ssh://git@" + base
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "ssh" || u.Hostname() == "" {
		return "", fmt.Errorf("%q: write host/organization, e.g. codeberg.org/your-org", raw)
	}
	org := strings.Trim(u.Path, "/")
	if err := validateForgeOrg(org); err != nil {
		return "", fmt.Errorf("%q: %w", raw, err)
	}
	if u.User == nil || u.User.Username() == "" {
		u.User = url.User("git")
	}
	return "ssh://" + u.User.Username() + "@" + u.Host + "/" + org, nil
}

// displayUpstream drops the ssh://git@ prefix for display.
func displayUpstream(baseURL string) string {
	return strings.TrimPrefix(baseURL, "ssh://git@")
}

// clientHostOptions are the addresses a client could be told to use: the
// node's address and name and every extra certificate name, then localhost.
// A kind cluster publishes on 127.0.0.1 only, so it offers localhost alone.
func clientHostOptions(st *WizardState) []option {
	local := option{value: "localhost", label: "localhost", detail: "this machine only"}
	if st.Config.CreateKind || st.ClusterInfo.engine == "kind" {
		local.detail = "kind publishes Oberth on 127.0.0.1 only"
		return []option{local}
	}
	var opts []option
	seen := map[string]bool{"localhost": true, "127.0.0.1": true}
	addOpt := func(value, detail string) {
		value = strings.ToLower(value)
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		opts = append(opts, option{value: value, label: value, detail: detail})
	}
	if api := apiServerHost(st); api != "" {
		addOpt(api, "the Kubernetes API server's host")
	}
	addOpt(st.ClusterInfo.nodeIP, "the node's address · machines that can reach this host, such as a VM's host or your LAN")
	addOpt(st.ClusterInfo.nodeName, "the node's name · other machines need it in DNS or /etc/hosts")
	for _, name := range st.Config.TLSExtraIPs {
		addOpt(name, "an address on the certificate")
	}
	for _, name := range st.Config.TLSExtraDNSNames {
		addOpt(name, "a name on the certificate")
	}
	// A .local name goes after the addresses that need no mDNS (#066).
	var mdns []option
	opts = slices.DeleteFunc(opts, func(o option) bool {
		if !mdnsdial.IsLocalName(o.value) {
			return false
		}
		o.detail += " · mDNS: avahi may also advertise the k3s bridge (10.42.x); the LAN IP is safer"
		mdns = append(mdns, o)
		return true
	})
	return append(append(opts, mdns...), local)
}

// defaultClientHost is the preselected client address: the stored answer,
// else what the installer would advertise, except that a .local name gives
// way to the node's address when that is offered (#066).
func defaultClientHost(st *WizardState) string {
	if st.Config.ClientHost != "" {
		return st.Config.ClientHost
	}
	host := clientHost(st)
	if mdnsdial.IsLocalName(host) && st.ClusterInfo.nodeIP != "" {
		for _, o := range clientHostOptions(st) {
			if o.value == strings.ToLower(st.ClusterInfo.nodeIP) {
				return o.value
			}
		}
	}
	return host
}

// apiServerHost is the kubeconfig server's host when it is not a loopback
// address: what the installer advertises by default for a remote cluster.
func apiServerHost(st *WizardState) string {
	u, err := url.Parse(st.ClusterInfo.server)
	if err != nil || u.Hostname() == "" || u.Hostname() == "localhost" {
		return ""
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() {
		return ""
	}
	return u.Hostname()
}

// tlsCertificateDescription says what certificate the server will serve: a
// production secret store gets short-lived leaves from the server PKI, other
// stores a self-signed certificate (#007).
func tlsCertificateDescription(state *WizardState) string {
	if state != nil && state.StoreMode == storeInstallProd {
		return "30-day ECDSA P-256 certificate from a local CA in OpenBao, renewed automatically"
	}
	return "self-signed certificate (ed25519)"
}

// serverCertificateNames is the name set the server certificate covers.
func serverCertificateNames(state *WizardState) (dnsNames, ips []string) {
	var extra []string
	extra = append(extra, state.Config.TLSExtraDNSNames...)
	extra = append(extra, state.Config.TLSExtraIPs...)
	return secretstore.ServerTLSNames(state.namespace(), extra)
}

// forgeUpstreamURL derives the upstream host/organization from the forge
// selection. Returns "" for unknown forge types.
func forgeUpstreamURL(forgeType, org string) string {
	if org == "" {
		return ""
	}
	switch forgeType {
	case "github":
		return "github.com/" + org
	case "codeberg":
		return "codeberg.org/" + org
	case "gitlab":
		return "gitlab.com/" + org
	default:
		return ""
	}
}

// forgeKeyDestination is where the forge accepts an SSH public key (#002).
func forgeKeyDestination(forgeType, org string) string {
	switch forgeType {
	case "github":
		return "https://github.com/settings/ssh/new on an account with access to " + org +
			" (or a deploy key per repository: https://github.com/" + org + "/<repo>/settings/keys/new)"
	case "codeberg":
		return "https://codeberg.org/user/settings/keys on an account with access to " + org
	case "gitlab":
		return "https://gitlab.com/-/user_settings/ssh_keys on an account with access to " + org
	}
	return "your forge account's SSH keys"
}

// ---------- the question table ----------

// buildQuestions returns every question in walk order. Steps without
// questions (review, install) are screens of their own.
func buildQuestions() []*question {
	isFile := func(st *WizardState) bool { return st.ClusterChoice == clusterChoiceFile }
	return []*question{
		{
			id: "cluster.target", step: stepCluster, heading: "Where will Oberth run?",
			explain: func(*WizardState) string {
				contexts, _, err := loadKubeContexts("")
				switch {
				case err != nil:
					return "Your kubeconfig could not be read (" + err.Error() + "). Point at a kubeconfig file instead."
				case len(contexts) == 0:
					return "No Kubernetes cluster configured. " + noClusterAdvice()
				}
				return "Choose the Kubernetes cluster to install into. Setup connects to it before moving on, so an unreachable cluster is caught here, not during the install."
			},
			options: func(*WizardState) []option {
				contexts, current, _ := loadKubeContexts("")
				opts := contextOptions(contexts, current)
				opts = append(opts, option{value: clusterChoiceFile, label: "Use a kubeconfig file…", detail: "point at another kubeconfig and choose one of its contexts"})
				if kindAvailable() {
					opts = append(opts, option{value: clusterChoiceKind, label: "Create a local kind cluster", detail: "kind and Docker are installed · cluster \"oberth\", context " + installer.KindContextName})
				}
				return opts
			},
			selected: func(st *WizardState) string {
				if st.ClusterChoice != "" {
					return st.ClusterChoice
				}
				_, current, _ := loadKubeContexts("")
				return current
			},
			save: func(st *WizardState, answer []string) error {
				choice := answer[0]
				switch choice {
				case "":
					return errors.New("choose a cluster")
				case clusterChoiceKind:
					if !kindAvailable() {
						return errors.New("kind and docker must both be installed to create a kind cluster")
					}
					st.Config.CreateKind = true
					st.Config.KubeconfigPath = ""
					st.Config.ContextName = ""
					st.ClusterInfo = clusterInfoMsg{context: installer.KindContextName, engine: "kind", isLocal: true}
				case clusterChoiceFile:
					st.Config.CreateKind = false
				default:
					contexts, _, err := loadKubeContexts("")
					if err != nil {
						return err
					}
					if !slices.ContainsFunc(contexts, func(c kubeContext) bool { return c.name == choice }) {
						return fmt.Errorf("context %q is not in your kubeconfig", choice)
					}
					st.Config.CreateKind = false
					st.Config.KubeconfigPath = ""
					st.Config.ContextName = choice
				}
				st.ClusterChoice = choice
				return nil
			},
			probe: func(st *WizardState) func() probeResult {
				if st.ClusterChoice == clusterChoiceKind || st.ClusterChoice == clusterChoiceFile {
					return nil
				}
				return probeContext(st.Config.ContextName, "")
			},
		},
		{
			id: "cluster.file", step: stepCluster, heading: "Which kubeconfig file?",
			explain: func(*WizardState) string {
				return "The path of a kubeconfig for an existing cluster. Its contexts are listed on the next screen."
			},
			applies: isFile,
			fields:  []string{"Kubeconfig file"}, placeholders: []string{"~/.kube/config"},
			values: func(st *WizardState) []string { return []string{displayHome(st.Config.KubeconfigPath)} },
			save: func(st *WizardState, answer []string) error {
				path, err := expandedPath(answer[0])
				if err != nil {
					return err
				}
				raw, err := clientcmd.LoadFromFile(path)
				if err != nil {
					return fmt.Errorf("read kubeconfig: %w", err)
				}
				if len(raw.Contexts) == 0 {
					return fmt.Errorf("%s has no contexts", displayHome(path))
				}
				if st.Config.KubeconfigPath != path {
					st.Config.ContextName = ""
				}
				st.Config.KubeconfigPath = path
				return nil
			},
		},
		{
			id: "cluster.context", step: stepCluster, heading: "Which context?",
			explain: func(st *WizardState) string {
				return "Contexts in " + displayHome(st.Config.KubeconfigPath) + ". Setup connects to the one you choose before moving on."
			},
			applies: isFile,
			options: func(st *WizardState) []option {
				contexts, current, _ := loadKubeContexts(st.Config.KubeconfigPath)
				return contextOptions(contexts, current)
			},
			selected: func(st *WizardState) string {
				if st.Config.ContextName != "" {
					return st.Config.ContextName
				}
				_, current, _ := loadKubeContexts(st.Config.KubeconfigPath)
				return current
			},
			save: func(st *WizardState, answer []string) error {
				contexts, _, err := loadKubeContexts(st.Config.KubeconfigPath)
				if err != nil {
					return err
				}
				if !slices.ContainsFunc(contexts, func(c kubeContext) bool { return c.name == answer[0] }) {
					return fmt.Errorf("context %q is not in %s", answer[0], displayHome(st.Config.KubeconfigPath))
				}
				st.Config.ContextName = answer[0]
				return nil
			},
			probe: func(st *WizardState) func() probeResult {
				return probeContext(st.Config.ContextName, st.Config.KubeconfigPath)
			},
		},
		{
			id: "cluster.namespaces", step: stepCluster, heading: "Name the namespaces.",
			explain: func(*WizardState) string {
				return "Oberth itself, the CI pipelines and the secret store each get their own namespace. Names must differ; " + namespaceRule + "."
			},
			fields:       []string{"Oberth", "Pipelines", "OpenBao"},
			placeholders: []string{installer.DefaultNamespace, installer.DefaultArgoNamespace, installer.DefaultOpenBaoNamespace},
			values: func(st *WizardState) []string {
				or := func(v, d string) string {
					if v == "" {
						return d
					}
					return v
				}
				return []string{
					or(st.Config.Namespace, installer.DefaultNamespace),
					or(st.Config.ArgoNamespace, installer.DefaultArgoNamespace),
					or(st.Config.OpenBaoNamespace, installer.DefaultOpenBaoNamespace),
				}
			},
			save: func(st *WizardState, answer []string) error {
				labels := []string{"Oberth", "Pipelines", "OpenBao"}
				for i, v := range answer {
					if !dns1123LabelRegexp.MatchString(v) {
						return fmt.Errorf("%s namespace: %s", labels[i], namespaceRule)
					}
				}
				if answer[0] == answer[1] || answer[0] == answer[2] || answer[1] == answer[2] {
					return errors.New("the three namespaces must differ")
				}
				st.Config.Namespace, st.Config.ArgoNamespace, st.Config.OpenBaoNamespace = answer[0], answer[1], answer[2]
				return nil
			},
		},
		{
			id: "cluster.network", step: stepCluster, heading: "Restrict CI network egress?",
			explain: func(*WizardState) string {
				return "A NetworkPolicy can firewall what CI jobs reach. k3s's built-in kube-router mishandles it, which is why auto leaves it off there."
			},
			options: func(*WizardState) []option {
				return []option{
					{value: "auto", label: "auto", detail: "enforce where the network plugin supports it"},
					{value: "true", label: "strict", detail: "always enforce"},
					{value: "false", label: "off", detail: "never enforce"},
				}
			},
			selected: func(st *WizardState) string {
				if st.Config.NetworkPolicy == "" {
					return "auto"
				}
				return st.Config.NetworkPolicy
			},
			save: func(st *WizardState, answer []string) error {
				np, ok := canonicalNetworkPolicy(answer[0])
				if !ok {
					return errors.New("network policy must be auto, strict, or off")
				}
				st.Config.NetworkPolicy = np
				return nil
			},
		},
		{
			id: "store.mode", step: stepStore, heading: "Where do release secrets live?",
			explain: func(*WizardState) string {
				return "Release secrets, the upstream SSH key and the server's TLS authority are kept in OpenBao. Production stores them persistently; you receive its root token and unseal key once, at the end of the install."
			},
			options: func(*WizardState) []option {
				return []option{
					{value: storeInstallProd, label: "Install OpenBao — production", detail: "persistent storage in this cluster · you hold the unseal key"},
					{value: storeInstallDev, label: "Install OpenBao — dev", detail: "in memory, unseals itself · evaluation only, lost on restart"},
					{value: storeConnect, label: "Connect an existing OpenBao / Vault", detail: "your own store, reachable over https"},
				}
			},
			selected: func(st *WizardState) string { return st.StoreMode },
			save: func(st *WizardState, answer []string) error {
				switch answer[0] {
				case storeInstallProd, storeInstallDev, storeConnect:
					st.setStoreMode(answer[0])
					return nil
				}
				return errors.New("choose a secret store")
			},
		},
		{
			id: "store.address", step: stepStore, heading: "Address of your OpenBao / Vault",
			explain: func(*WizardState) string {
				return "Oberth signs in with its own Kubernetes identity (role oberth-secretstore) and never asks for an admin token — grant the role with setup-secretstore.sh. After the install, set the CA with `helm upgrade oberth … --set argo.vault.caCert=…` and approve secret paths with `oberth access allow <repo> <step> <path>`."
			},
			applies: func(st *WizardState) bool { return st.StoreMode == storeConnect },
			fields:  []string{"Address"}, placeholders: []string{"https://vault.example.com:8200"},
			values: func(st *WizardState) []string { return []string{st.StoreAddress} },
			save: func(st *WizardState, answer []string) error {
				addr := strings.TrimSpace(answer[0])
				if err := validateStoreAddress(addr); err != nil {
					return err
				}
				st.StoreAddress = addr
				st.Config.ArgoVaultAddress = addr
				return nil
			},
		},
		{
			id: "store.anchoring", step: stepStore, heading: "Anchor the audit log externally?",
			explain: func(*WizardState) string {
				return "A local Rekor transparency log can witness Oberth's audit chain. OpenBao keeps its signing key and database credentials."
			},
			applies: func(st *WizardState) bool { return st.StoreMode == storeInstallProd },
			options: func(*WizardState) []option {
				return []option{
					{value: "off", label: "No", detail: "the audit chain is verified by Oberth alone"},
					{value: "on", label: "Yes, install a local Rekor witness", detail: "adds MySQL, Trillian and Rekor to the cluster"},
				}
			},
			selected: func(st *WizardState) string {
				if st.Config.InstallRekor {
					return "on"
				}
				return "off"
			},
			save: func(st *WizardState, answer []string) error {
				st.Config.InstallRekor = answer[0] == "on"
				return nil
			},
		},
		{
			id: "tls.names", step: stepTLS, heading: "Which addresses will clients use?",
			explain: func(st *WizardState) string {
				dnsNames, ips := secretstore.ServerTLSNames(st.namespace(), nil)
				text := "Oberth serves a " + tlsCertificateDescription(st) + ". It is always valid for " +
					strings.Join(append(dnsNames, ips...), ", ") + ". Add every name or IP address clients reach this server by — " +
					"a name missing here appears later as a hostname mismatch no client can fix."
				if st.Config.CreateKind {
					text += " localhost is added for a kind cluster."
				}
				return text
			},
			fields: []string{"Extra names"}, placeholders: []string{"node.example.com, 192.0.2.10"},
			values: func(st *WizardState) []string {
				names := append(slices.Clone(st.Config.TLSExtraDNSNames), st.Config.TLSExtraIPs...)
				if len(names) == 0 && !st.done[stepTLS] {
					if st.ClusterInfo.nodeName != "" {
						names = append(names, strings.ToLower(st.ClusterInfo.nodeName))
					}
					if st.ClusterInfo.nodeIP != "" {
						names = append(names, st.ClusterInfo.nodeIP)
					}
				}
				return []string{strings.Join(names, ", ")}
			},
			save: func(st *WizardState, answer []string) error {
				var dnsNames, ips []string
				for _, name := range splitNames(answer[0]) {
					if ip := net.ParseIP(name); ip != nil {
						if !slices.Contains(ips, ip.String()) {
							ips = append(ips, ip.String())
						}
						continue
					}
					name = strings.ToLower(strings.TrimSuffix(name, "."))
					if len(name) > 253 || !dnsNameRegexp.MatchString(name) {
						return fmt.Errorf("%q is neither an IP address nor a DNS name", name)
					}
					if !slices.Contains(dnsNames, name) {
						dnsNames = append(dnsNames, name)
					}
				}
				st.Config.TLSExtraDNSNames, st.Config.TLSExtraIPs = dnsNames, ips
				return nil
			},
		},
		{
			id: "tls.client", step: stepTLS, heading: "Which address do clients use?",
			explain: func(st *WizardState) string {
				if st.Config.CreateKind || st.ClusterInfo.engine == "kind" {
					return "kind publishes ports 30022 and 30443 on 127.0.0.1 only, so clients on this machine use localhost."
				}
				text := "Clone URLs, the CLI and MCP settings and the dashboard link name this address, and the certificate covers it. On k3s and most clusters, ports 30022 and 30443 already listen on every address of the node, so pick the one your other machines (a laptop, the VM's host) reach this server by. localhost works on this machine only."
				if warning := installer.LocalNameWarning(defaultClientHost(st)); warning != "" {
					text += " " + warning
				}
				return text
			},
			options:  clientHostOptions,
			selected: defaultClientHost,
			save: func(st *WizardState, answer []string) error {
				for _, o := range clientHostOptions(st) {
					if o.value == answer[0] {
						st.Config.ClientHost = answer[0]
						return nil
					}
				}
				return fmt.Errorf("%s is not one of this server's addresses any more; choose again", answer[0])
			},
		},
		{
			id: "forge.type", step: stepForge, heading: "Where does green code go?",
			explain: func(*WizardState) string {
				return "Oberth publishes green branches to an upstream forge and mirrors its repositories."
			},
			options: func(*WizardState) []option {
				return []option{
					{value: "github", label: "GitHub", detail: "github.com"},
					{value: "codeberg", label: "Codeberg", detail: "codeberg.org"},
					{value: "gitlab", label: "GitLab", detail: "gitlab.com"},
				}
			},
			selected: func(st *WizardState) string { return st.ForgeType },
			save: func(st *WizardState, answer []string) error {
				if forgeUpstreamURL(answer[0], "x") == "" {
					return errors.New("choose GitHub, Codeberg or GitLab")
				}
				st.ForgeType = answer[0]
				return nil
			},
		},
		{
			id: "forge.org", step: stepForge, heading: "Which organization?",
			explain: func(st *WizardState) string {
				return "The owner or organization that holds your repositories. Oberth connects to the whole organization; repositories are then cloned as " +
					st.ForgeType + "/<organization>/<repo>."
			},
			fields: []string{"Organization"}, placeholders: []string{"your-org"},
			values: func(st *WizardState) []string { return []string{st.ForgeOrg} },
			save: func(st *WizardState, answer []string) error {
				org := strings.TrimSpace(answer[0])
				if err := validateForgeOrg(org); err != nil {
					return err
				}
				st.ForgeOrg = org
				return nil
			},
		},
		{
			id: "forge.key", step: stepForge, heading: "How does Oberth authenticate to the forge?",
			explain: func(st *WizardState) string {
				return "This is the upstream key: Oberth → " + st.ForgeType + ". It is not your uplink key (workstation → Oberth), which the Uplink step asks for. The key is kept in OpenBao."
			},
			options: func(*WizardState) []option {
				return []option{
					{value: forgeAuthGenerate, label: "Generate a new upstream SSH key", detail: "kept in OpenBao only · after the install, add its public key to your forge account (every repository you can reach, private ones too) or as a deploy key (one repository)"},
					{value: forgeAuthExisting, label: "Use an existing SSH key from this machine", detail: "already added to your forge account · no passphrase · imported into OpenBao"},
				}
			},
			selected: func(st *WizardState) string { return st.ForgeAuth },
			save: func(st *WizardState, answer []string) error {
				switch answer[0] {
				case forgeAuthGenerate:
					st.Config.UpstreamPrivateKeyPath = ""
					st.UpstreamKeyPick = ""
				case forgeAuthExisting:
				default:
					return errors.New("choose how Oberth authenticates")
				}
				st.ForgeAuth = answer[0]
				return nil
			},
		},
		{
			id: "forge.keypick", step: stepForge, heading: "Which private key?",
			explain: func(st *WizardState) string {
				text := "Your work SSH identity: Oberth → " + st.ForgeType + ". The private key is imported into OpenBao, so it must have no passphrase; setup checks the one you choose. Listed: keys in ~/.ssh with a matching .pub."
				if len(scanSSHKeyPairs()) == 0 {
					text += " No key pair was found in ~/.ssh. In another terminal run  ssh-keygen -t ed25519 -N '' -f ~/.ssh/oberth_upstream  and add ~/.ssh/oberth_upstream.pub at " +
						forgeKeyDestination(st.ForgeType, st.ForgeOrg) + ", then choose Scan again. Or go back and let Oberth generate the key."
				}
				return text
			},
			applies: func(st *WizardState) bool { return st.ForgeAuth == forgeAuthExisting },
			options: func(*WizardState) []option {
				var opts []option
				for _, k := range scanSSHKeyPairs() {
					detail := k.algorithm + " · " + k.fingerprint
					if k.comment != "" {
						detail += " · " + k.comment
					}
					opts = append(opts, option{value: k.private, label: displayHome(k.private), detail: detail})
				}
				return append(opts,
					option{value: upstreamKeyRescan, label: "Scan ~/.ssh again", detail: "after creating a key in another terminal"},
					option{value: upstreamKeyOther, label: "Another file…", detail: "name a private key file"})
			},
			selected: func(st *WizardState) string {
				if st.UpstreamKeyPick != "" {
					return st.UpstreamKeyPick
				}
				return st.Config.UpstreamPrivateKeyPath
			},
			save: func(st *WizardState, answer []string) error {
				switch answer[0] {
				case upstreamKeyRescan:
					return errRescan
				case upstreamKeyOther:
					st.UpstreamKeyPick = upstreamKeyOther
					return nil
				case "":
					return errors.New("choose a private key")
				}
				if err := installer.CheckUpstreamPrivateKey(answer[0]); err != nil {
					return err
				}
				st.UpstreamKeyPick = answer[0]
				st.Config.UpstreamPrivateKeyPath = answer[0]
				return nil
			},
		},
		{
			id: "forge.keyfile", step: stepForge, heading: "Which private key file?",
			explain: func(*WizardState) string {
				return "The path of the private key (not the .pub). It is imported into OpenBao, so it must not have a passphrase; setup checks it now."
			},
			applies: func(st *WizardState) bool {
				return st.ForgeAuth == forgeAuthExisting && st.UpstreamKeyPick == upstreamKeyOther
			},
			fields: []string{"Private key"}, placeholders: []string{"~/.ssh/<private key file>"},
			values: func(st *WizardState) []string { return []string{displayHome(st.Config.UpstreamPrivateKeyPath)} },
			save: func(st *WizardState, answer []string) error {
				path, err := expandedPath(answer[0])
				if err != nil {
					return err
				}
				info, err := os.Stat(path)
				if err != nil || !info.Mode().IsRegular() {
					return fmt.Errorf("%s is not a key file", displayHome(path))
				}
				if err := installer.CheckUpstreamPrivateKey(path); err != nil {
					return err
				}
				st.Config.UpstreamPrivateKeyPath = path
				return nil
			},
		},
		{
			id: "forge.more", step: stepForge, heading: "Connect more organizations?",
			explain: func(st *WizardState) string {
				return "Optional. Each is connected with the same upstream key, so that key must also be registered with those accounts (a key on GitHub does not open Codeberg). Separate several with commas; leave empty to skip. More can be added after the install."
			},
			fields: []string{"Also connect"}, placeholders: []string{"codeberg.org/your-org, github.com/another-org"},
			values: func(st *WizardState) []string {
				shown := make([]string, 0, len(st.Config.AlsoUpstreams))
				for _, u := range st.Config.AlsoUpstreams {
					shown = append(shown, displayUpstream(u))
				}
				return []string{strings.Join(shown, ", ")}
			},
			save: func(st *WizardState, answer []string) error {
				primary := "ssh://git@" + forgeUpstreamURL(st.ForgeType, st.ForgeOrg)
				var also []string
				for _, raw := range splitNames(answer[0]) {
					u, err := normalizeAlsoUpstream(raw)
					if err != nil {
						return err
					}
					if u == primary {
						return fmt.Errorf("%s is the organization chosen above", displayUpstream(u))
					}
					if !slices.Contains(also, u) {
						also = append(also, u)
					}
				}
				st.Config.AlsoUpstreams = also
				return nil
			},
		},
		{
			id: "uplink.identity", step: stepUplink, heading: "Who are you?",
			explain: func(*WizardState) string {
				return "Your uplink identity, name@machine. Every push and every CLI call from this workstation is linked to it."
			},
			fields: []string{"Identity"}, placeholders: []string{"name@machine"},
			values: func(st *WizardState) []string {
				if st.UplinkIdentity == "" {
					return []string{suggestedIdentity()}
				}
				return []string{st.UplinkIdentity}
			},
			save: func(st *WizardState, answer []string) error {
				identity := strings.TrimSpace(answer[0])
				if err := validateUplinkIdentityTUI(identity); err != nil {
					return err
				}
				st.UplinkIdentity = identity
				return nil
			},
		},
		{
			id: "uplink.key", step: stepUplink, heading: "Which SSH key do you push with?",
			explain: func(*WizardState) string {
				text := "The uplink key: workstation → Oberth. Only public keys (*.pub) are read."
				if len(scanSSHPublicKeys()) == 0 {
					text += " No public keys were found in ~/.ssh — create one with `ssh-keygen -t ed25519` and revisit this screen, or name a file."
				}
				return text
			},
			options: func(st *WizardState) []option {
				var opts []option
				for _, k := range scanSSHPublicKeys() {
					detail := k.algorithm + " · " + k.fingerprint
					if strings.TrimSuffix(k.path, ".pub") == st.Config.UpstreamPrivateKeyPath {
						detail += " · also your upstream key (Oberth → forge)"
					}
					opts = append(opts, option{value: k.path, label: filepath.Base(k.path), detail: detail})
				}
				return append(opts, option{value: uplinkKeyOther, label: "Another public key file…", detail: "name a *.pub file"})
			},
			selected: func(st *WizardState) string {
				if st.UplinkKey != "" {
					return st.UplinkKey
				}
				return st.SSHKeyPath
			},
			save: func(st *WizardState, answer []string) error {
				if answer[0] == uplinkKeyOther {
					st.UplinkKey = uplinkKeyOther
					return nil
				}
				if answer[0] == "" {
					return errors.New("choose a public key")
				}
				if _, err := checkPublicKeyFile(answer[0]); err != nil {
					return err
				}
				st.UplinkKey = answer[0]
				st.SSHKeyPath = answer[0]
				return nil
			},
		},
		{
			id: "uplink.keyfile", step: stepUplink, heading: "Which public key file?",
			explain: func(*WizardState) string {
				return "The public half (*.pub) of the key you push with. The private key is never read."
			},
			applies: func(st *WizardState) bool { return st.UplinkKey == uplinkKeyOther },
			fields:  []string{"Public key"}, placeholders: []string{"~/.ssh/id_ed25519.pub"},
			values: func(st *WizardState) []string { return []string{displayHome(st.SSHKeyPath)} },
			save: func(st *WizardState, answer []string) error {
				path, err := expandedPath(answer[0])
				if err != nil {
					return err
				}
				if _, err := checkPublicKeyFile(path); err != nil {
					return err
				}
				st.SSHKeyPath = path
				return nil
			},
		},
		{
			id: "client.access", step: stepClient, heading: "Configure this machine's clients?",
			explain: func(*WizardState) string {
				dir, err := installer.ClientConfigDir()
				if err != nil {
					dir = "~/.config/oberth"
				}
				read, _ := installer.TokenCommands()
				return "After the install, setup writes the server's CA and client settings to " + displayHome(dir) +
					": env for the oberth CLI, mcp.json for MCP clients (registered with Claude Code when it is installed). " +
					"No token is written to these files; they read it with `" + read + "`."
			},
			options: func(*WizardState) []option {
				return []option{
					{value: "both", label: "CLI and MCP", detail: "env, mcp.json and ca.crt"},
					{value: "cli", label: "CLI only", detail: "env and ca.crt"},
					{value: "mcp", label: "MCP only", detail: "mcp.json and ca.crt"},
					{value: "none", label: "Neither", detail: "configure clients by hand later"},
				}
			},
			selected: func(st *WizardState) string { return st.ClientAccess },
			save: func(st *WizardState, answer []string) error {
				if answer[0] == "" {
					return errors.New("choose which clients to configure")
				}
				if err := installer.ValidClientAccess(answer[0]); err != nil {
					return err
				}
				st.ClientAccess = answer[0]
				return nil
			},
		},
	}
}

// questionsOf returns a step's questions in order.
func questionsOf(all []*question, step stepID) []*question {
	var qs []*question
	for _, q := range all {
		if q.step == step {
			qs = append(qs, q)
		}
	}
	return qs
}

// revalidate re-checks every saved answer of the question steps, so answers
// resumed from setup.json — or made stale by a later change — are caught on
// the review screen, before the install. It returns the first failing
// question.
func revalidate(all []*question, st *WizardState) (*question, error) {
	for _, q := range all {
		if !q.appliesTo(st) {
			continue
		}
		if !st.done[q.step] {
			return q, fmt.Errorf("step %d, %s, is not done yet", int(q.step)+1, q.step.name())
		}
		if err := q.save(st, q.answerOf(st)); err != nil {
			return q, err
		}
	}
	return nil, nil
}
