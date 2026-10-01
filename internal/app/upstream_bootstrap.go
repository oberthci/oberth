package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

const (
	maximumBootstrapMaterialBytes = 1 << 20
	maximumConfirmationBytes      = 4 << 10
	bootstrapFieldManager         = "oberth-upstream-bootstrap"
	defaultSSHProbeTimeout        = 15 * time.Second
)

var ErrUpstreamBootstrapIncomplete = errors.New("app: upstream bootstrap is incomplete")

// ErrInterrupted is returned when the operator presses Ctrl+C (ETX / 0x03)
// during an interactive confirmation prompt. Callers should propagate it to
// produce a clean exit without an error dump.
var ErrInterrupted = errors.New("interrupted")

// UpstreamKeyFieldManager derives the per-upstream server-side-apply field
// manager for the shared upstream-key Secret. Each upstream owns exactly its
// own data keys under its own manager, so applying one upstream's key can
// never prune another upstream's (or the shared fallback's) key material.
// The upstream name is ValidateUpstreamName-restricted, keeping the manager
// a plain ASCII identifier.
func UpstreamKeyFieldManager(upstreamName string) string {
	return bootstrapFieldManager + "-" + upstreamName
}

// SSHHostKeyScanner obtains the public host keys offered by one SSH endpoint.
// The result is untrusted until the operator confirms its fingerprints.
type SSHHostKeyScanner func(context.Context, string, string) ([]ssh.PublicKey, error)

// SSHAuthProbe proves that an existing identity can authenticate to the
// upstream SSH endpoint before configuration becomes live. A successful
// handshake asserts endpoint reachability, pinned host-key verification,
// and accepted identity. Write access is verified on the first publication.
// Implementations must never log the privateKey argument.
type SSHAuthProbe func(context.Context, string, []byte, []byte) error

// UpstreamSSHBootstrap ensures that the strict SSH identity used for an
// upstream is either already projected or durably stored in name-scoped
// Kubernetes Secrets. KubernetesClient is lazy so a fully configured install
// does not need to contact the API during an ordinary upstream registration.
type UpstreamSSHBootstrap struct {
	Input  io.Reader
	Output io.Writer

	KubernetesClient func() (kubernetes.Interface, error)
	ScanHostKeys     SSHHostKeyScanner
	Probe            SSHAuthProbe
	Random           io.Reader
	// MutationGate is the live daemon's fail-closed external audit gate. It is
	// invoked immediately before each Kubernetes Secret patch.
	MutationGate func(context.Context, string) error

	Namespace         string
	PrivateKeySecret  string
	KnownHostsSecret  string
	PrivateKeyPath    string
	KnownHostsPath    string
	PrivateKeyDataKey string
	PublicKeyDataKey  string
	KnownHostsDataKey string
	// KeyFieldManager overrides the server-side-apply field manager for the
	// private-key Secret apply only. Per-upstream registrations must supply a
	// manager unique to the upstream: server-side apply prunes every field a
	// manager previously owned but no longer applies, so re-using one shared
	// manager for the shared Secret would delete every other upstream's key
	// (and the shared fallback key) on each registration. Empty selects the
	// shared default manager; the known-hosts apply always uses the shared
	// manager because known_hosts is a single merged document.
	KeyFieldManager string

	// Registration polling: after printing a generated or Secret-recovered
	// deploy key whose authentication probe does not yet succeed, Ensure waits
	// for the operator to register the key on the forge instead of demanding
	// a rerun. ProbeInterval defaults to 10s and ProbeWait to 10m; NoWait
	// restores the immediate exit for non-interactive automation. The
	// fully-projected fast path never waits — a previously working identity
	// that stops probing is an incident signal, not an onboarding state.
	ProbeInterval           time.Duration
	ProbeWait               time.Duration
	NoWait                  bool
	AutoYes                 bool
	ExpectedHostFingerprint string
}

// UpstreamSSHIdentity is safe to display. It deliberately contains no private
// key material.
type UpstreamSSHIdentity struct {
	PublicKey   []byte
	Fingerprint string
	Generated   bool
}

type privateIdentity struct {
	privateKey  []byte
	publicKey   []byte
	fingerprint string
}

type secretMaterial struct {
	privateKey []byte
	publicKey  []byte
	knownHosts []byte
}

type knownHostsMaterial struct {
	body []byte
	keys []ssh.PublicKey
}

// Ensure validates configured files first. If they are unavailable, it
// recovers persisted material or performs an explicitly confirmed bootstrap.
func (bootstrap UpstreamSSHBootstrap) Ensure(ctx context.Context, baseURL string) (UpstreamSSHIdentity, error) {
	if err := bootstrap.validate(); err != nil {
		return UpstreamSSHIdentity{}, err
	}
	host, port, address, err := sshEndpoint(baseURL)
	if err != nil {
		return UpstreamSSHIdentity{}, err
	}

	projectedPrivate, projectedPrivateErr := readPrivateIdentityFile(bootstrap.PrivateKeyPath)
	projectedHosts, projectedHostsErr := readKnownHostsFile(bootstrap.KnownHostsPath, address)
	if projectedPrivateErr == nil && projectedHostsErr == nil && len(projectedHosts.keys) > 0 {
		if err := bootstrap.Probe(ctx, baseURL, projectedPrivate.privateKey, projectedHosts.body); err != nil {
			if displayErr := bootstrap.printPersistedIdentity(projectedPrivate, host, port); displayErr != nil {
				return UpstreamSSHIdentity{}, displayErr
			}
			return UpstreamSSHIdentity{}, fmt.Errorf("app: upstream SSH authentication probe failed: %w", err)
		}
		return projectedPrivate.publicIdentity(false), nil
	}
	var confirmations *confirmationReader
	if bootstrap.AutoYes {
		confirmations = newConfirmationReader(strings.NewReader("yes\nyes\nyes\n"))
	} else {
		confirmations = newConfirmationReader(bootstrap.Input)
	}

	client, err := bootstrap.KubernetesClient()
	if err != nil {
		return UpstreamSSHIdentity{}, fmt.Errorf("app: create Kubernetes client for upstream bootstrap: %w", err)
	}
	persisted, err := bootstrap.loadPersisted(ctx, client)
	if err != nil {
		return UpstreamSSHIdentity{}, err
	}

	identity, generated, recovered, err := bootstrap.selectPrivateIdentity(confirmations, projectedPrivate, projectedPrivateErr, persisted)
	if err != nil {
		return UpstreamSSHIdentity{}, err
	}

	knownHosts, hostKeys, err := selectKnownHosts(projectedHosts, projectedHostsErr, persisted.knownHosts, address)
	if err != nil {
		return UpstreamSSHIdentity{}, err
	}
	acquiredHosts := len(hostKeys) == 0
	if acquiredHosts {
		hostKeys, err = bootstrap.acquireHostKeys(ctx, confirmations, host, port)
		if err != nil {
			return UpstreamSSHIdentity{}, err
		}
		knownHosts = appendKnownHosts(knownHosts, address, hostKeys)
		if _, err := knownHostKeys(knownHosts, address); err != nil {
			return UpstreamSSHIdentity{}, fmt.Errorf("app: validate confirmed known_hosts: %w", err)
		}
	}

	if acquiredHosts {
		if err := bootstrap.applySecret(ctx, client, bootstrap.KnownHostsSecret, map[string][]byte{
			bootstrap.KnownHostsDataKey: knownHosts,
		}, bootstrapFieldManager); err != nil {
			return UpstreamSSHIdentity{}, err
		}
		if err := bootstrap.verifySecretData(ctx, client, bootstrap.KnownHostsSecret, map[string][]byte{
			bootstrap.KnownHostsDataKey: knownHosts,
		}); err != nil {
			return UpstreamSSHIdentity{}, err
		}
	}
	if generated {
		if err := bootstrap.applySecret(ctx, client, bootstrap.PrivateKeySecret, map[string][]byte{
			bootstrap.PrivateKeyDataKey: identity.privateKey,
			bootstrap.PublicKeyDataKey:  identity.publicKey,
		}, bootstrap.KeyFieldManager); err != nil {
			return UpstreamSSHIdentity{}, err
		}
		if err := bootstrap.verifySecretData(ctx, client, bootstrap.PrivateKeySecret, map[string][]byte{
			bootstrap.PrivateKeyDataKey: identity.privateKey,
			bootstrap.PublicKeyDataKey:  identity.publicKey,
		}); err != nil {
			return UpstreamSSHIdentity{}, err
		}
		if err := bootstrap.printGeneratedIdentity(identity, host, port); err != nil {
			return UpstreamSSHIdentity{}, err
		}
		if bootstrap.waitForRegistration(ctx, baseURL, host, port, identity.privateKey, knownHosts) {
			return identity.publicIdentity(true), nil
		}
		return identity.publicIdentity(true), fmt.Errorf("%w: register the printed public key with the upstream, then rerun upstream add", ErrUpstreamBootstrapIncomplete)
	}
	advertised := false
	if recovered {
		if err := bootstrap.printPersistedIdentity(identity, host, port); err != nil {
			return UpstreamSSHIdentity{}, err
		}
		advertised = true
	}
	if err := bootstrap.Probe(ctx, baseURL, identity.privateKey, knownHosts); err != nil {
		if !advertised {
			if displayErr := bootstrap.printPersistedIdentity(identity, host, port); displayErr != nil {
				return UpstreamSSHIdentity{}, displayErr
			}
		}
		if bootstrap.NoWait {
			return UpstreamSSHIdentity{}, fmt.Errorf("app: upstream SSH authentication probe failed: %w", err)
		}
		if !bootstrap.waitForRegistration(ctx, baseURL, host, port, identity.privateKey, knownHosts) {
			return UpstreamSSHIdentity{}, fmt.Errorf("%w: the persisted key was not accepted in time (%w); register it, then rerun upstream add", ErrUpstreamBootstrapIncomplete, err)
		}
	}
	return identity.publicIdentity(false), nil
}

// waitForRegistration polls the SSH authentication probe until the operator
// registers the just-printed deploy key on the forge, the wait budget runs
// out, or the command is canceled. It reports whether the upstream accepted
// the key. Probing uses the exact persisted material, so a success here is
// the same proof a rerun would have produced.
//
// Output is a single growing line of dots — the publickey auth rejection is
// the expected state (the key is not registered yet), so it is silent. Every
// other error (host-key mismatch, network faults) produces a warning line:
// silencing an unknown error here would also silence an active MITM
// indicator, so classification is a strict allowlist of the one expected
// failure. On timeout the last probe error is printed so the session itself
// carries the diagnosis.
func (bootstrap UpstreamSSHBootstrap) waitForRegistration(ctx context.Context, baseURL, host, port string, privateKey, knownHosts []byte) bool {
	if bootstrap.NoWait {
		return false
	}
	interval := bootstrap.ProbeInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	wait := bootstrap.ProbeWait
	if wait <= 0 {
		wait = 10 * time.Minute
	}
	_, _ = fmt.Fprintf(bootstrap.Output, "\nWaiting for key registration on %s ", host)
	started := time.Now()
	var lastErr error
	for {
		remaining := wait - time.Since(started)
		if remaining <= 0 {
			_, _ = fmt.Fprintf(bootstrap.Output, " timed out\n")
			if lastErr != nil {
				_, _ = fmt.Fprintf(bootstrap.Output, "  last probe error: %s\n", terseProbeError(lastErr))
			}
			return false
		}
		select {
		case <-ctx.Done():
			_, _ = fmt.Fprintf(bootstrap.Output, " stopped waiting\n")
			return false
		case <-time.After(min(interval, remaining)):
		}
		err := bootstrap.Probe(ctx, baseURL, privateKey, knownHosts)
		if err == nil {
			_, _ = fmt.Fprintf(bootstrap.Output, " Key authenticated\n")
			_, _ = fmt.Fprintf(bootstrap.Output, "  Write access is verified on the first publication.\n")
			return true
		}
		if ctx.Err() != nil {
			_, _ = fmt.Fprintf(bootstrap.Output, " stopped waiting\n")
			return false
		}
		lastErr = err
		if isExpectedAuthRejection(err) {
			_, _ = fmt.Fprint(bootstrap.Output, "·")
		} else {
			_, _ = fmt.Fprintf(bootstrap.Output, "\n  warning: %s\nWaiting for key registration on %s ", terseProbeError(err), host)
		}
	}
}

// isExpectedAuthRejection reports whether the probe error is the normal
// "key not registered yet" SSH publickey rejection — the only error class
// that is both expected and uninformative while waiting for the operator to
// register the key. Matching is a deliberate allowlist on the x/crypto/ssh
// auth-failure signature ("ssh: unable to authenticate, attempted methods
// [...], no supported methods remain"). Everything else is surfaced by the
// caller: a knownhosts mismatch is an active MITM indicator, a handshake EOF
// means the endpoint is not an SSH forge. Classifying by denylist here would
// silence exactly those signals.
func isExpectedAuthRejection(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unable to authenticate") ||
		strings.Contains(msg, "no supported methods remain")
}

// terseProbeError compresses a probe failure to one bounded line for the
// warning message shown when an unexpected (non-auth-rejection) error occurs
// and for the timed-out summary.
func terseProbeError(err error) string {
	text := strings.Join(strings.Fields(err.Error()), " ")
	if len(text) > 160 {
		text = text[:160] + "..."
	}
	return text
}

func (bootstrap UpstreamSSHBootstrap) validate() error {
	if bootstrap.Input == nil || bootstrap.Output == nil || bootstrap.KubernetesClient == nil || bootstrap.ScanHostKeys == nil || bootstrap.Probe == nil {
		return errors.New("app: upstream bootstrap input, output, Kubernetes client, host-key scanner, and authentication probe are required")
	}
	if bootstrap.PrivateKeySecret == bootstrap.KnownHostsSecret {
		return errors.New("app: upstream private-key and known-hosts Secrets must be distinct")
	}
	for kind, value := range map[string]string{
		"namespace":          bootstrap.Namespace,
		"private-key Secret": bootstrap.PrivateKeySecret,
		"known-hosts Secret": bootstrap.KnownHostsSecret,
	} {
		if problems := validation.IsDNS1123Subdomain(value); len(problems) != 0 {
			return fmt.Errorf("app: invalid %s %q: %s", kind, value, strings.Join(problems, "; "))
		}
	}
	for kind, value := range map[string]string{
		"private-key data key": bootstrap.PrivateKeyDataKey,
		"public-key data key":  bootstrap.PublicKeyDataKey,
		"known-hosts data key": bootstrap.KnownHostsDataKey,
	} {
		if problems := validation.IsConfigMapKey(value); len(problems) != 0 {
			return fmt.Errorf("app: invalid %s %q: %s", kind, value, strings.Join(problems, "; "))
		}
	}
	if err := validateOperatorPath(bootstrap.PrivateKeyPath); err != nil {
		return fmt.Errorf("app: upstream private-key path: %w", err)
	}
	if err := validateOperatorPath(bootstrap.KnownHostsPath); err != nil {
		return fmt.Errorf("app: upstream known-hosts path: %w", err)
	}
	return nil
}

func (bootstrap UpstreamSSHBootstrap) selectPrivateIdentity(confirmations *confirmationReader, projected privateIdentity, projectedErr error, persisted secretMaterial) (privateIdentity, bool, bool, error) {
	if projectedErr == nil {
		return projected, false, false, nil
	}
	if len(persisted.privateKey) > 0 {
		identity, err := parsePrivateIdentity(persisted.privateKey)
		if err != nil {
			return privateIdentity{}, false, false, fmt.Errorf("app: persisted upstream private key is invalid; refusing to replace it: %w", err)
		}
		if len(persisted.publicKey) > 0 {
			matches, err := sameAuthorizedKey(persisted.publicKey, identity.publicKey)
			if err != nil {
				return privateIdentity{}, false, false, fmt.Errorf("app: persisted upstream public key is invalid: %w", err)
			}
			if !matches {
				return privateIdentity{}, false, false, errors.New("app: persisted upstream public key does not match its private key")
			}
		}
		return identity, false, true, nil
	}
	if projectedErr != nil && !errors.Is(projectedErr, os.ErrNotExist) {
		return privateIdentity{}, false, false, fmt.Errorf("app: configured upstream private key is invalid; refusing to replace it: %w", projectedErr)
	}
	accepted, err := confirmations.confirmShort(bootstrap.Output,
		fmt.Sprintf("No upstream SSH private key is configured. Generate an Ed25519 key and store it in Secret %s/%s? [y/N]: ", bootstrap.Namespace, bootstrap.PrivateKeySecret))
	if err != nil {
		return privateIdentity{}, false, false, err
	}
	if !accepted {
		return privateIdentity{}, false, false, errors.New("app: upstream SSH key generation was not accepted")
	}
	identity, err := generatePrivateIdentity(bootstrap.Random)
	if err != nil {
		return privateIdentity{}, false, false, err
	}
	return identity, true, false, nil
}

func selectKnownHosts(projected knownHostsMaterial, projectedErr error, persisted []byte, address string) ([]byte, []ssh.PublicKey, error) {
	if projectedErr == nil && len(projected.keys) > 0 {
		return append([]byte(nil), projected.body...), append([]ssh.PublicKey(nil), projected.keys...), nil
	}
	if len(persisted) > 0 {
		keys, err := knownHostKeys(persisted, address)
		if err != nil {
			return nil, nil, fmt.Errorf("app: persisted upstream known_hosts is invalid; refusing to replace it: %w", err)
		}
		if len(keys) > 0 {
			return append([]byte(nil), persisted...), keys, nil
		}
		return append([]byte(nil), persisted...), nil, nil
	}
	if projectedErr != nil && !errors.Is(projectedErr, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("app: configured upstream known_hosts is invalid; refusing to replace it: %w", projectedErr)
	}
	if projectedErr == nil {
		return append([]byte(nil), projected.body...), nil, nil
	}
	return nil, nil, nil
}

// wellKnownForges pins the SHA-256 SSH host-key fingerprints published by
// each forge. Keep these in sync with the forge-operated documentation:
//   - https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/githubs-ssh-key-fingerprints
//   - https://docs.codeberg.org/security/ssh-fingerprint/
//   - https://docs.gitlab.com/user/gitlab_com/#ssh-host-keys-fingerprints
//   - https://support.atlassian.com/bitbucket-cloud/docs/configure-ssh-and-two-step-verification/#SSHHostKeys
var wellKnownForges = map[string]map[string]struct{}{
	"github.com": {
		"SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU": {}, // Ed25519
		"SHA256:uNiVztksCsDhcc0u9e8BujQXVUpKZIDTMczCvj3tD2s": {}, // RSA
		"SHA256:p2QAMXNIC1TJYWeIOttrVc98/R1BUFWu3/LiyKgUfQM": {}, // ECDSA
	},
	"codeberg.org": {
		"SHA256:mIlxA9k46MmM6qdJOdMnAQpzGxF4WIVVL+fj+wZbw0g": {}, // Ed25519
		"SHA256:6QQmYi4ppFS4/+zSZ5S4IU+4sa6rwvQ4PbhCtPEBekQ": {}, // RSA
		"SHA256:T9FYDEHELhVkulEKKwge5aVhVTbqCW0MIRwAfpARs/E": {}, // ECDSA
	},
	"gitlab.com": {
		"SHA256:eUXGGm1YGsMAS7vkcx6JOJdOGHPem5gQp4taiCfCLB8": {}, // Ed25519
		"SHA256:ROQFvPThGrW4RuWLoL9tq9I9zJ42fK4XywyRtbOz/EQ": {}, // RSA
		"SHA256:HbW3g8zUjNSksFbqTiUWPWg2Bq1x8xdGUrliXFzSnUw": {}, // ECDSA
	},
	"bitbucket.org": {
		"SHA256:ybgmFkzwOSotHTHLJgHO0QN8L0xErw6vd0VhFA9m3SM": {}, // Ed25519
		"SHA256:46OSHA1Rmj8E8ERTC6xkNcmGOw9oFxYr0WF6zWW8l1E": {}, // RSA
		"SHA256:FC73VB6C4OQLSCrjEayhMp9UMxS97caD/Yyi2bhW/J0": {}, // ECDSA
	},
}

func (bootstrap UpstreamSSHBootstrap) acquireHostKeys(ctx context.Context, confirmations *confirmationReader, host, port string) ([]ssh.PublicKey, error) {
	keys, err := bootstrap.ScanHostKeys(ctx, host, port)
	if err != nil {
		return nil, fmt.Errorf("app: scan SSH host keys for %s:%s: %w", host, port, err)
	}
	keys = uniqueHostKeys(keys)
	if len(keys) == 0 {
		return nil, fmt.Errorf("app: SSH host %s:%s returned no supported host keys", host, port)
	}
	// Auto-accept well-known forges only after every key returned by the scan
	// matches a fingerprint published by that forge. A valid key must not hide
	// an additional unpinned key in the same response.
	if publishedFingerprints, ok := wellKnownForges[strings.TrimSuffix(strings.ToLower(host), ".")]; ok {
		for _, key := range keys {
			if _, published := publishedFingerprints[ssh.FingerprintSHA256(key)]; !published {
				return nil, fmt.Errorf("MITM WARNING: %s SSH fingerprint does not match published value", host)
			}
		}
		return keys, nil
	}
	// When an expected fingerprint is provided (--expected-host-fingerprint),
	// validate all scanned keys against it without interactive confirmation.
	// This is the only unattended path for unknown forges: --yes alone is not
	// sufficient because it would convert an unauthenticated ssh-keyscan result
	// into a permanent host pin, allowing a network/DNS attacker to become the
	// trusted forge.
	if bootstrap.ExpectedHostFingerprint != "" {
		var matched []ssh.PublicKey
		for _, key := range keys {
			if ssh.FingerprintSHA256(key) == bootstrap.ExpectedHostFingerprint {
				matched = append(matched, key)
			}
		}
		if len(matched) == 0 {
			scanned := make([]string, 0, len(keys))
			for _, key := range keys {
				scanned = append(scanned, ssh.FingerprintSHA256(key))
			}
			return nil, fmt.Errorf("app: SSH host-key fingerprint mismatch for %s:%s: expected %s, scanned %s",
				host, port, bootstrap.ExpectedHostFingerprint, strings.Join(scanned, ", "))
		}
		return matched, nil
	}
	// AutoYes may confirm key generation but must never auto-accept unknown
	// SSH host keys: pass --expected-host-fingerprint SHA256:... instead.
	if bootstrap.AutoYes {
		return nil, fmt.Errorf("app: --yes cannot trust unauthenticated SSH host keys for unknown forge %s:%s; use --expected-host-fingerprint SHA256:... to pin the expected fingerprint", host, port)
	}
	if _, err := fmt.Fprintf(bootstrap.Output, "\nSSH host-key fingerprints offered by %s:%s:\n", host, port); err != nil {
		return nil, err
	}
	for _, key := range keys {
		if _, err := fmt.Fprintf(bootstrap.Output, "- %s %s\n", key.Type(), ssh.FingerprintSHA256(key)); err != nil {
			return nil, err
		}
	}
	accepted, err := confirmations.confirm(bootstrap.Output,
		"Verify these fingerprints through a trusted channel. Type 'yes' to trust them for strict SSH host-key checking: ")
	if err != nil {
		return nil, err
	}
	if !accepted {
		return nil, errors.New("app: upstream SSH host keys were not accepted")
	}
	return keys, nil
}

func (bootstrap UpstreamSSHBootstrap) printGeneratedIdentity(identity privateIdentity, host, port string) error {
	_, err := fmt.Fprintf(bootstrap.Output,
		"\nAdd this deploy key to your %s account with WRITE access:\n\n%s\n",
		host, identity.publicKey)
	return err
}

func (bootstrap UpstreamSSHBootstrap) printPersistedIdentity(identity privateIdentity, host, port string) error {
	_, err := fmt.Fprintf(bootstrap.Output,
		"\nPersisted upstream deploy public key (%s):\n\n%s\nEnsure this public key is registered for %s:%s with repository write access.\n",
		identity.fingerprint, identity.publicKey, host, port)
	return err
}

func (bootstrap UpstreamSSHBootstrap) loadPersisted(ctx context.Context, client kubernetes.Interface) (secretMaterial, error) {
	privateSecret, err := client.CoreV1().Secrets(bootstrap.Namespace).Get(ctx, bootstrap.PrivateKeySecret, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return secretMaterial{}, fmt.Errorf("app: read upstream private-key Secret %s/%s: %w", bootstrap.Namespace, bootstrap.PrivateKeySecret, err)
	}
	knownHostsSecret, err := client.CoreV1().Secrets(bootstrap.Namespace).Get(ctx, bootstrap.KnownHostsSecret, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return secretMaterial{}, fmt.Errorf("app: read upstream known-hosts Secret %s/%s: %w", bootstrap.Namespace, bootstrap.KnownHostsSecret, err)
	}
	var material secretMaterial
	if privateSecret != nil {
		material.privateKey = append([]byte(nil), privateSecret.Data[bootstrap.PrivateKeyDataKey]...)
		material.publicKey = append([]byte(nil), privateSecret.Data[bootstrap.PublicKeyDataKey]...)
	}
	if knownHostsSecret != nil {
		material.knownHosts = append([]byte(nil), knownHostsSecret.Data[bootstrap.KnownHostsDataKey]...)
	}
	return material, nil
}

func (bootstrap UpstreamSSHBootstrap) applySecret(ctx context.Context, client kubernetes.Interface, name string, data map[string][]byte, fieldManager string) error {
	if bootstrap.MutationGate == nil {
		return errors.New("app: audit mutation gate is required before applying an upstream Secret")
	}
	if fieldManager == "" {
		fieldManager = bootstrapFieldManager
	}
	if err := bootstrap.MutationGate(ctx, "upstream.secret."+name); err != nil {
		return fmt.Errorf("app: audit mutation gate before upstream Secret %s/%s: %w", bootstrap.Namespace, name, err)
	}
	secret := corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: bootstrap.Namespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
	body, err := json.Marshal(secret)
	if err != nil {
		return fmt.Errorf("app: encode upstream Secret apply: %w", err)
	}
	// The chart's zero-prerequisite install pre-creates these Secrets with an
	// empty data map, so the "helm" field manager owns .data before the first
	// bootstrap write, and every later `helm upgrade` re-claims it via the
	// lookup-preserving template. The bootstrap is the authoritative writer of
	// exactly the fields it applies (audit-gated immediately above), so it must
	// force server-side-apply conflicts as Kubernetes prescribes for
	// controllers; a non-forced apply fails closed against the placeholder.
	force := true
	if _, err := client.CoreV1().Secrets(bootstrap.Namespace).Patch(ctx, name, types.ApplyPatchType, body, metav1.PatchOptions{
		FieldManager: fieldManager,
		Force:        &force,
	}); err != nil {
		return fmt.Errorf("app: apply upstream Secret %s/%s: %w", bootstrap.Namespace, name, err)
	}
	return nil
}

func (bootstrap UpstreamSSHBootstrap) verifySecretData(ctx context.Context, client kubernetes.Interface, name string, expected map[string][]byte) error {
	secret, err := client.CoreV1().Secrets(bootstrap.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("app: verify upstream Secret %s/%s: %w", bootstrap.Namespace, name, err)
	}
	for key, value := range expected {
		if !bytes.Equal(secret.Data[key], value) {
			return fmt.Errorf("app: upstream Secret %s/%s did not retain data key %s", bootstrap.Namespace, name, key)
		}
	}
	return nil
}

func readPrivateIdentityFile(path string) (privateIdentity, error) {
	if err := validateOperatorFile(path, true); err != nil {
		return privateIdentity{}, err
	}
	body, err := readBoundedFileForBootstrap("upstream private key", path, nil)
	if err != nil {
		return privateIdentity{}, err
	}
	return parsePrivateIdentity(body)
}

func parsePrivateIdentity(body []byte) (privateIdentity, error) {
	if len(body) == 0 || len(body) > maximumBootstrapMaterialBytes {
		return privateIdentity{}, errors.New("private key must be non-empty and no larger than 1 MiB")
	}
	signer, err := ssh.ParsePrivateKey(body)
	if err != nil {
		return privateIdentity{}, fmt.Errorf("parse SSH private key: %w", err)
	}
	publicKey := append(bytes.TrimSpace(ssh.MarshalAuthorizedKey(signer.PublicKey())), []byte(" oberth\n")...)
	return privateIdentity{
		privateKey:  append([]byte(nil), body...),
		publicKey:   publicKey,
		fingerprint: ssh.FingerprintSHA256(signer.PublicKey()),
	}, nil
}

func sameAuthorizedKey(left, right []byte) (bool, error) {
	leftKey, _, leftOptions, leftRest, err := ssh.ParseAuthorizedKey(left)
	if err != nil {
		return false, err
	}
	if len(leftOptions) != 0 || len(bytes.TrimSpace(leftRest)) != 0 {
		return false, errors.New("public-key data must contain exactly one key without authorization options")
	}
	rightKey, _, rightOptions, rightRest, err := ssh.ParseAuthorizedKey(right)
	if err != nil {
		return false, err
	}
	if len(rightOptions) != 0 || len(bytes.TrimSpace(rightRest)) != 0 {
		return false, errors.New("expected public key is malformed")
	}
	return bytes.Equal(leftKey.Marshal(), rightKey.Marshal()), nil
}

func generatePrivateIdentity(random io.Reader) (privateIdentity, error) {
	if random == nil {
		random = cryptorand.Reader
	}
	_, privateKey, err := ed25519.GenerateKey(random)
	if err != nil {
		return privateIdentity{}, fmt.Errorf("app: generate Ed25519 upstream key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(privateKey, "oberth upstream")
	if err != nil {
		return privateIdentity{}, fmt.Errorf("app: encode Ed25519 upstream key: %w", err)
	}
	return parsePrivateIdentity(pem.EncodeToMemory(block))
}

func (identity privateIdentity) publicIdentity(generated bool) UpstreamSSHIdentity {
	return UpstreamSSHIdentity{
		PublicKey:   append([]byte(nil), identity.publicKey...),
		Fingerprint: identity.fingerprint,
		Generated:   generated,
	}
}

func readKnownHostsFile(path, address string) (knownHostsMaterial, error) {
	if err := validateOperatorFile(path, false); err != nil {
		return knownHostsMaterial{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return knownHostsMaterial{}, err
	}
	if info.Mode().Perm()&0o022 != 0 {
		return knownHostsMaterial{}, errors.New("known_hosts must not be writable by group or other")
	}
	body, err := readBoundedFileForBootstrap("upstream known_hosts", path, nil)
	if err != nil {
		return knownHostsMaterial{}, err
	}
	keys, err := knownHostKeys(body, address)
	if err != nil {
		return knownHostsMaterial{}, err
	}
	return knownHostsMaterial{body: body, keys: keys}, nil
}

func readBoundedFileForBootstrap(kind, path string, reader io.Reader) ([]byte, error) {
	if reader == nil {
		// #nosec G304 -- validated absolute operator paths select projected Secret files.
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer func() { _ = file.Close() }()
		reader = file
	}
	body, err := io.ReadAll(io.LimitReader(reader, maximumBootstrapMaterialBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", kind, err)
	}
	if len(body) == 0 || len(body) > maximumBootstrapMaterialBytes {
		return nil, fmt.Errorf("%s must be non-empty and no larger than 1 MiB", kind)
	}
	return body, nil
}

func knownHostKeys(body []byte, address string) ([]ssh.PublicKey, error) {
	callback, cleanup, err := newKnownHostsCallback(body)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	var matches []ssh.PublicKey
	rest := body
	for {
		_, _, key, _, next, parseErr := ssh.ParseKnownHosts(rest)
		if errors.Is(parseErr, io.EOF) {
			break
		}
		if parseErr != nil {
			return nil, parseErr
		}
		if callback(address, stringAddress(address), key) == nil {
			matches = append(matches, key)
		}
		rest = next
	}
	return uniqueHostKeys(matches), nil
}

func newKnownHostsCallback(body []byte) (ssh.HostKeyCallback, func(), error) {
	if len(body) == 0 || len(body) > maximumBootstrapMaterialBytes {
		return nil, nil, errors.New("known_hosts must be non-empty and no larger than 1 MiB")
	}
	file, err := os.CreateTemp("", "oberth-known-hosts-")
	if err != nil {
		return nil, nil, err
	}
	path := file.Name()
	cleanup := func() { _ = os.Remove(path) }
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		cleanup()
		return nil, nil, err
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		cleanup()
		return nil, nil, err
	}
	if err := file.Close(); err != nil {
		cleanup()
		return nil, nil, err
	}
	callback, err := knownhosts.New(path)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return callback, cleanup, nil
}

func appendKnownHosts(existing []byte, address string, keys []ssh.PublicKey) []byte {
	result := append([]byte(nil), bytes.TrimSpace(existing)...)
	if len(result) > 0 {
		result = append(result, '\n')
	}
	for _, key := range uniqueHostKeys(keys) {
		result = append(result, knownhosts.Line([]string{address}, key)...)
		result = append(result, '\n')
	}
	return result
}

func uniqueHostKeys(values []ssh.PublicKey) []ssh.PublicKey {
	seen := make(map[string]struct{}, len(values))
	result := make([]ssh.PublicKey, 0, len(values))
	for _, key := range values {
		if key == nil {
			continue
		}
		encoded := string(key.Marshal())
		if _, ok := seen[encoded]; ok {
			continue
		}
		seen[encoded] = struct{}{}
		result = append(result, key)
	}
	return result
}

func sshEndpoint(baseURL string) (string, string, string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", "", "", fmt.Errorf("app: parse SSH upstream: %w", err)
	}
	if parsed.Scheme != "ssh" || parsed.Hostname() == "" {
		return "", "", "", errors.New("app: SSH bootstrap requires an ssh:// upstream URL")
	}
	host := parsed.Hostname()
	port := parsed.Port()
	if port == "" {
		port = "22"
	}
	value, err := strconv.Atoi(port)
	if err != nil || value < 1 || value > 65535 {
		return "", "", "", errors.New("app: SSH upstream port must be between 1 and 65535")
	}
	return host, port, net.JoinHostPort(host, port), nil
}

type confirmationReader struct {
	scanner *bufio.Scanner
}

func newConfirmationReader(input io.Reader) *confirmationReader {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 128), maximumConfirmationBytes)
	return &confirmationReader{scanner: scanner}
}

// readAnswer prints the prompt and returns the operator's answer, trimmed
// and lowercased. EOF yields an empty answer (rejection) without an error.
// An ETX byte (0x03, Ctrl+C) anywhere in the scanned line is treated as an
// interrupt — the operator pressed Ctrl+C but the terminal delivered the byte
// as data instead of generating SIGINT (common when signal.NotifyContext has
// claimed SIGINT).
func (reader *confirmationReader) readAnswer(output io.Writer, prompt string) (string, error) {
	if _, err := io.WriteString(output, prompt); err != nil {
		return "", err
	}
	if !reader.scanner.Scan() {
		if err := reader.scanner.Err(); err != nil {
			return "", fmt.Errorf("app: read bootstrap confirmation: %w", err)
		}
		return "", nil
	}
	text := reader.scanner.Text()
	if strings.ContainsRune(text, 0x03) {
		return "", ErrInterrupted
	}
	return strings.ToLower(strings.TrimSpace(text)), nil
}

// confirm requires the full word "yes" — deliberate friction reserved for
// trust decisions, today the acceptance of scanned SSH host keys that become
// a permanent pin. A stray "y" keystroke must never pin an unverified host
// key; OpenSSH's own strict host-key prompt rejects "y" for the same reason.
func (reader *confirmationReader) confirm(output io.Writer, prompt string) (bool, error) {
	answer, err := reader.readAnswer(output, prompt)
	return answer == "yes", err
}

// confirmShort also accepts a single "y". It is for low-stakes confirmations
// only (generating a fresh keypair); trust decisions use confirm.
func (reader *confirmationReader) confirmShort(output io.Writer, prompt string) (bool, error) {
	answer, err := reader.readAnswer(output, prompt)
	return answer == "yes" || answer == "y", err
}

// ScanSSHHostKeys uses the OpenSSH scanner shipped in the server image. It
// returns public keys only; callers must display and explicitly confirm their
// fingerprints before persisting them.
func ScanSSHHostKeys(ctx context.Context, host, port string) ([]ssh.PublicKey, error) {
	if host == "" || strings.HasPrefix(host, "-") || strings.ContainsAny(host, "\x00\r\n") {
		return nil, errors.New("invalid SSH host")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return nil, errors.New("invalid SSH port")
	}
	// #nosec G204 -- host and numeric port are parsed from a validated ssh:// URL and passed without a shell.
	command := exec.CommandContext(ctx, "ssh-keyscan", "-T", "10", "-p", port, "-t", "ed25519,ecdsa,rsa", host)
	var stdout, stderr cappedBuffer
	stdout.limit = maximumBootstrapMaterialBytes
	stderr.limit = 8 << 10
	command.Stdout = &stdout
	command.Stderr = &stderr
	runErr := command.Run()
	if stdout.overflow {
		return nil, errors.New("ssh-keyscan output exceeded 1 MiB")
	}
	keys, parseErr := parseScannedHostKeys(stdout.Bytes())
	if parseErr != nil {
		return nil, parseErr
	}
	if len(keys) > 0 {
		return keys, nil
	}
	if runErr != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return nil, fmt.Errorf("ssh-keyscan failed: %w (%s)", runErr, detail)
		}
		return nil, fmt.Errorf("ssh-keyscan failed: %w", runErr)
	}
	return nil, errors.New("ssh-keyscan returned no supported host keys")
}

// ProbeSSHAuthentication performs a bare SSH handshake to assert endpoint
// reachability, pinned host-key verification, and accepted identity. It
// opens no channels and executes no commands — the forge accepts the key if
// the handshake completes. Write access is verified on the first publication.
func ProbeSSHAuthentication(ctx context.Context, baseURL string, privateKey, knownHosts []byte) error {
	if err := ValidateUpstreamBase(baseURL); err != nil {
		return err
	}
	_, _, address, err := sshEndpoint(baseURL)
	if err != nil {
		return err
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("parse SSH upstream for authentication probe: %w", err)
	}
	user := "git"
	if parsed.User != nil && parsed.User.Username() != "" {
		user = parsed.User.Username()
	}
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return fmt.Errorf("parse SSH identity for authentication probe: %w", err)
	}
	hostKeyCallback, cleanup, err := newKnownHostsCallback(knownHosts)
	if err != nil {
		return fmt.Errorf("load strict known_hosts for authentication probe: %w", err)
	}
	defer cleanup()

	probeCtx, cancel := context.WithTimeout(ctx, defaultSSHProbeTimeout)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(probeCtx, "tcp", address)
	if err != nil {
		return fmt.Errorf("connect to SSH upstream: %w", err)
	}
	defer func() { _ = connection.Close() }()
	if deadline, ok := probeCtx.Deadline(); ok {
		if err := connection.SetDeadline(deadline); err != nil {
			return fmt.Errorf("bound SSH authentication probe: %w", err)
		}
	}
	clientConnection, channels, requests, err := ssh.NewClientConn(connection, address, &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostKeyCallback,
	})
	if err != nil {
		return fmt.Errorf("authenticate to SSH upstream: %w", err)
	}
	client := ssh.NewClient(clientConnection, channels, requests)
	_ = client.Close()
	return nil
}

func parseScannedHostKeys(body []byte) ([]ssh.PublicKey, error) {
	var result []ssh.PublicKey
	rest := body
	for {
		marker, _, key, _, next, err := ssh.ParseKnownHosts(rest)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse ssh-keyscan output: %w", err)
		}
		if marker != "" {
			return nil, errors.New("ssh-keyscan returned an unexpected known_hosts marker")
		}
		result = append(result, key)
		rest = next
	}
	return uniqueHostKeys(result), nil
}

type cappedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (buffer *cappedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	remaining := buffer.limit - buffer.Len()
	if remaining <= 0 {
		buffer.overflow = true
		return original, nil
	}
	if len(value) > remaining {
		buffer.overflow = true
		value = value[:remaining]
	}
	_, _ = buffer.Buffer.Write(value)
	return original, nil
}

type stringAddress string

func (address stringAddress) Network() string { return "tcp" }
func (address stringAddress) String() string  { return string(address) }
