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

// SSHHostKeyScanner obtains the public host keys offered by one SSH endpoint.
// The result is untrusted until the operator confirms its fingerprints.
type SSHHostKeyScanner func(context.Context, string, string) ([]ssh.PublicKey, error)

// SSHCapabilityProbe proves that an existing identity can authenticate and
// request the upstream's Git upload capability before configuration becomes
// live. Implementations must never log the privateKey argument.
type SSHCapabilityProbe func(context.Context, string, []byte, []byte) error

// UpstreamSSHBootstrap ensures that the strict SSH identity used for an
// upstream is either already projected or durably stored in name-scoped
// Kubernetes Secrets. KubernetesClient is lazy so a fully configured install
// does not need to contact the API during an ordinary upstream registration.
type UpstreamSSHBootstrap struct {
	Input  io.Reader
	Output io.Writer

	KubernetesClient func() (kubernetes.Interface, error)
	ScanHostKeys     SSHHostKeyScanner
	Probe            SSHCapabilityProbe
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
			return UpstreamSSHIdentity{}, fmt.Errorf("app: authenticated upstream SSH capability probe failed: %w", err)
		}
		return projectedPrivate.publicIdentity(false), nil
	}
	confirmations := newConfirmationReader(bootstrap.Input)

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
		}); err != nil {
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
		}); err != nil {
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
		return UpstreamSSHIdentity{}, fmt.Errorf("app: authenticated upstream SSH capability probe failed: %w", err)
	}
	return identity.publicIdentity(false), nil
}

func (bootstrap UpstreamSSHBootstrap) validate() error {
	if bootstrap.Input == nil || bootstrap.Output == nil || bootstrap.KubernetesClient == nil || bootstrap.ScanHostKeys == nil || bootstrap.Probe == nil {
		return errors.New("app: upstream bootstrap input, output, Kubernetes client, host-key scanner, and capability probe are required")
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
	accepted, err := confirmations.confirm(bootstrap.Output,
		fmt.Sprintf("No upstream SSH private key is configured. Generate an Ed25519 key and store it in Secret %s/%s? Type 'yes' to continue: ", bootstrap.Namespace, bootstrap.PrivateKeySecret))
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

func (bootstrap UpstreamSSHBootstrap) acquireHostKeys(ctx context.Context, confirmations *confirmationReader, host, port string) ([]ssh.PublicKey, error) {
	keys, err := bootstrap.ScanHostKeys(ctx, host, port)
	if err != nil {
		return nil, fmt.Errorf("app: scan SSH host keys for %s:%s: %w", host, port, err)
	}
	keys = uniqueHostKeys(keys)
	if len(keys) == 0 {
		return nil, fmt.Errorf("app: SSH host %s:%s returned no supported host keys", host, port)
	}
	if _, err := fmt.Fprintf(bootstrap.Output, "SSH host-key fingerprints offered by %s:%s:\n", host, port); err != nil {
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
		"Generated upstream deploy public key (%s):\n%sAdd this public key to the SSH upstream for %s:%s with the required repository write access.\n",
		identity.fingerprint, identity.publicKey, host, port)
	return err
}

func (bootstrap UpstreamSSHBootstrap) printPersistedIdentity(identity privateIdentity, host, port string) error {
	_, err := fmt.Fprintf(bootstrap.Output,
		"Persisted upstream deploy public key (%s):\n%sEnsure this public key is registered for %s:%s; Oberth will now verify authenticated Git access.\n",
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

func (bootstrap UpstreamSSHBootstrap) applySecret(ctx context.Context, client kubernetes.Interface, name string, data map[string][]byte) error {
	if bootstrap.MutationGate == nil {
		return errors.New("app: audit mutation gate is required before applying an upstream Secret")
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
	if _, err := client.CoreV1().Secrets(bootstrap.Namespace).Patch(ctx, name, types.ApplyPatchType, body, metav1.PatchOptions{
		FieldManager: bootstrapFieldManager,
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

func (reader *confirmationReader) confirm(output io.Writer, prompt string) (bool, error) {
	if _, err := io.WriteString(output, prompt); err != nil {
		return false, err
	}
	if !reader.scanner.Scan() {
		if err := reader.scanner.Err(); err != nil {
			return false, fmt.Errorf("app: read bootstrap confirmation: %w", err)
		}
		return false, nil
	}
	return strings.EqualFold(strings.TrimSpace(reader.scanner.Text()), "yes"), nil
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

// ProbeSSHCapability authenticates with the configured key, verifies the
// pinned host key, and requests receive-pack for the real Oberth repository.
// Reading its initial Git advertisement proves write-command authorization;
// the session closes before sending an update, so the probe cannot mutate it.
func ProbeSSHCapability(ctx context.Context, baseURL string, privateKey, knownHosts []byte) error {
	if err := ValidateUpstreamBase(baseURL); err != nil {
		return err
	}
	_, _, address, err := sshEndpoint(baseURL)
	if err != nil {
		return err
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("parse SSH upstream for capability probe: %w", err)
	}
	user := "git"
	if parsed.User != nil && parsed.User.Username() != "" {
		user = parsed.User.Username()
	}
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return fmt.Errorf("parse SSH identity for capability probe: %w", err)
	}
	hostKeyCallback, cleanup, err := newKnownHostsCallback(knownHosts)
	if err != nil {
		return fmt.Errorf("load strict known_hosts for capability probe: %w", err)
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
			return fmt.Errorf("bound SSH capability probe: %w", err)
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
	defer func() { _ = client.Close() }()
	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("open authenticated SSH session: %w", err)
	}
	defer func() { _ = session.Close() }()
	stdout, err := session.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open Git write-capability probe output: %w", err)
	}
	var stderr cappedBuffer
	stderr.limit = 64 << 10
	session.Stderr = &stderr
	probeRepository := strings.TrimSuffix(parsed.Path, "/") + "/oberth.git"
	if err := session.Start("git-receive-pack " + shellQuote(probeRepository)); err != nil {
		return fmt.Errorf("request authenticated Git write capability: %w", err)
	}
	if err := readGitAdvertisement(stdout); err != nil {
		_ = session.Close()
		_ = session.Wait()
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return fmt.Errorf("verify authenticated Git write capability: %w (%s)", err, detail)
		}
		return fmt.Errorf("verify authenticated Git write capability: %w", err)
	}
	_ = session.Close()
	if err := probeCtx.Err(); err != nil {
		return fmt.Errorf("complete authenticated Git write-capability probe: %w", err)
	}
	return nil
}

func readGitAdvertisement(reader io.Reader) error {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return fmt.Errorf("read Git advertisement header: %w", err)
	}
	length, err := strconv.ParseUint(string(header[:]), 16, 16)
	if err != nil || length != 0 && (length < 4 || length > 65520) {
		return errors.New("remote returned an invalid Git advertisement")
	}
	if length == 0 {
		return nil
	}
	payload := make([]byte, int(length)-len(header))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return fmt.Errorf("read Git advertisement packet: %w", err)
	}
	if bytes.HasPrefix(payload, []byte("ERR ")) {
		return errors.New("remote rejected Git write access")
	}
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
