package installer

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// goProxyDNSNames returns the four DNS names a goproxy service requires on
// the server certificate. fullname is the Helm release fullname ("oberth").
func goProxyDNSNames(fullname, namespace string) []string {
	base := fullname + "-goproxy"
	return []string{
		base,
		base + "." + namespace,
		base + "." + namespace + ".svc",
		base + "." + namespace + ".svc.cluster.local",
	}
}

// liveHelmValuesSubset is the subset of a Helm release's values the identity
// preflight inspects: goProxy enablement and watchTunnel origin certificate.
type liveHelmValuesSubset struct {
	Argo struct {
		GoProxy struct {
			Enabled bool `json:"enabled"`
		} `json:"goProxy"`
	} `json:"argo"`
	WatchTunnel struct {
		Enabled      bool   `json:"enabled"`
		OriginCACert string `json:"originCACert"`
	} `json:"watchTunnel"`
}

// readLiveHelmValuesSubset reads the current Helm release values for the
// identity preflight. When deps.RunHelm is nil (tests that do not wire
// Helm), zero values are returned. When RunHelm is wired and returns an
// error, the error is propagated — a Helm failure must not silently disable
// the SAN check.
func readLiveHelmValuesSubset(ctx context.Context, deps Deps, ns string) (liveHelmValuesSubset, error) {
	var values liveHelmValuesSubset
	if deps.RunHelm == nil {
		return values, nil
	}
	out, err := deps.RunHelm(ctx, []string{"get", "values", "oberth", "-n", ns, "-o", "json"})
	if err != nil {
		return values, err
	}
	if len(out) > 0 {
		if err := json.Unmarshal(out, &values); err != nil {
			return values, fmt.Errorf("parse Helm values JSON: %w", err)
		}
	}
	return values, nil
}

// identityBundleSpec maps a legacy Secret name to its required OpenBao bundle.
// oberth-goproxy-tls is not mapped: its identity is absorbed by the server
// bundle (identity_store.go:189).
var legacySecretToBaoBundle = map[string]identityBundleSpec{
	"oberth-tls":          {baoName: "server", fields: []string{"tls.crt", "tls.key", "ssh_host_key"}},
	"oberth-ssh-host-key": {baoName: "server", fields: []string{"tls.crt", "tls.key", "ssh_host_key"}},
	"oberth-upstream-key": {baoName: "oberth-upstream-key", fields: []string{"id_ed25519", "id_ed25519.pub"}},
	"oberth-known-hosts":  {baoName: "oberth-known-hosts", fields: []string{"known_hosts"}},
}

type identityBundleSpec struct {
	baoName string
	fields  []string
}

// preflightServerIdentities verifies that server identities are present in
// OpenBao before the Helm upgrade replaces the Deployment. It runs on both the
// real and dry-run paths. When the server certificate lacks SANs required by
// the live goProxy configuration and an admin token is available, the
// certificate is rotated in place (CAS-bound write of tls.crt only).
func preflightServerIdentities(ctx context.Context, cfg *Config, deps Deps, dryRun bool) error {
	ns := cfg.Namespace
	if ns == "" {
		ns = DefaultNamespace
	}
	if deps.KubeClient == nil {
		return nil
	}

	deployment, err := deps.KubeClient.AppsV1().Deployments(ns).Get(ctx, "oberth", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil // fresh install — bootstrapRuntimeIdentities creates identities at first boot
	}
	if err != nil {
		return fmt.Errorf("check existing deployment: %w", err)
	}

	// Classify the deployment: legacy (Secret volumes) or current (OpenBao).
	var legacySecretNames []string
	for _, vol := range deployment.Spec.Template.Spec.Volumes {
		if vol.Secret != nil {
			legacySecretNames = append(legacySecretNames, vol.Secret.SecretName)
		}
	}
	hasLegacySecrets := len(legacySecretNames) > 0

	w := deps.Output

	// Resolve the admin token and OpenBao exec client.
	token := os.Getenv(baoTokenEnvVar)
	if token == "" && cfg.InstallSecretStoreDev {
		token = devRootToken
	}
	var store openBaoExec
	var haveStore bool
	var podLookupErr error

	openbaoNS := cfg.OpenBaoNamespace
	if openbaoNS == "" {
		openbaoNS = DefaultOpenBaoNamespace
	}

	if token != "" {
		pod, err := findOpenBaoPod(ctx, deps, openbaoNS)
		if err == nil {
			store = newOpenBaoExec(deps, openbaoNS, pod)
			haveStore = true
		} else {
			podLookupErr = err
		}
	}

	// -- Legacy shape: verify identity bundles in OpenBao before Helm --
	var serverData map[string]any
	var serverVersion int

	if hasLegacySecrets {
		// Refuse unknown Secret volumes before any OpenBao reads or token checks.
		var unknownVolumes []string
		for _, secretName := range legacySecretNames {
			if _, ok := legacySecretToBaoBundle[secretName]; !ok && secretName != "oberth-goproxy-tls" { // #nosec G101 — Secret object name, not a credential
				unknownVolumes = append(unknownVolumes, secretName)
			}
		}
		if len(unknownVolumes) > 0 {
			quoted := make([]string, len(unknownVolumes))
			for i, name := range unknownVolumes {
				quoted[i] = fmt.Sprintf("%q", name)
			}
			return fmt.Errorf("unsupported deployment shape: Secret volume %s is not a known "+
				"Oberth identity Secret; refusing to replace the Deployment",
				strings.Join(quoted, ", "))
		}

		if token == "" {
			return fmt.Errorf("the existing Oberth deployment uses Kubernetes Secret volumes for identities; " +
				"upgrading requires --install-secretstore with " + baoTokenEnvVar + " set so the installer " +
				"can verify the identities are seeded in OpenBao before replacing the Deployment")
		}
		if !haveStore {
			return fmt.Errorf(baoTokenEnvVar+" is set but the OpenBao pod in namespace %s could not be found: %w",
				openbaoNS, podLookupErr)
		}

		// Determine which bundles are needed (deduplicate by baoName).
		// The server bundle is always required for legacy shapes.
		needed := map[string]identityBundleSpec{
			"server": legacySecretToBaoBundle["oberth-tls"],
		}
		for _, secretName := range legacySecretNames {
			if bundle, ok := legacySecretToBaoBundle[secretName]; ok {
				needed[bundle.baoName] = bundle
			}
		}

		var missing []string
		for baoName, bundle := range needed {
			path := defaultKVPrefix + "/data/identities/" + ns + "/" + baoName
			result, readErr := store.readData(ctx, token, path)
			if readErr != nil {
				return fmt.Errorf("read identity bundle %s: %w", baoName, readErr)
			}
			if result == nil {
				missing = append(missing, fmt.Sprintf("%s/identities/%s/%s (%s)",
					defaultKVPrefix, ns, baoName, strings.Join(bundle.fields, ", ")))
				continue
			}
			dataMap, _ := result["data"].(map[string]any)
			for _, field := range bundle.fields {
				if v, ok := dataMap[field]; !ok || v == nil || v == "" {
					missing = append(missing, fmt.Sprintf("%s/identities/%s/%s field %q",
						defaultKVPrefix, ns, baoName, field))
				}
			}
			if baoName == "server" {
				serverData = dataMap
				if metaMap, ok := result["metadata"].(map[string]any); ok {
					if v, ok := metaMap["version"].(float64); ok {
						serverVersion = int(v)
					}
				}
			}
			if dryRun {
				_, _ = fmt.Fprintf(w, "  Identity preflight: %s bundle present (%s)\n",
					baoName, strings.Join(bundle.fields, ", "))
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("identities must be seeded in OpenBao before upgrading: missing %s",
				strings.Join(missing, "; "))
		}

		// Cross-check: the OpenBao server cert's public key must match the
		// running pod's cert public key. Comparing keys (not PEM bytes) allows
		// a partial-failure re-run where the cert was rotated in OpenBao (new
		// SANs) but the pod still serves the old cert with the same key.
		if serverData != nil {
			if baoCertPEM, ok := serverData["tls.crt"].(string); ok && baoCertPEM != "" {
				podCert, podErr := readPodServerCert(ctx, *cfg, deps)
				if podErr == nil {
					baoPubDER, baoFP, baoParseErr := parseCertPublicKey([]byte(baoCertPEM))
					if baoParseErr != nil {
						return fmt.Errorf("the server certificate in OpenBao is not valid: %w", baoParseErr)
					}
					podPubDER, podFP, podParseErr := parseCertPublicKey(podCert)
					if podParseErr != nil {
						return fmt.Errorf("the server certificate in the running pod is not valid: %w", podParseErr)
					}
					if !bytesEqual(baoPubDER, podPubDER) {
						return fmt.Errorf("the server identity key in OpenBao does not match "+
							"the running deployment; refusing to replace the trusted identity "+
							"(OpenBao cert %s, pod cert %s)", baoFP, podFP)
					}
				} else {
					_, _ = fmt.Fprintf(w, "WARNING: could not read the server certificate from the running pod; "+
						"cross-check skipped (%v)\n", podErr)
				}
			}

			// Print SSH host key public fingerprint (never the private key).
			if sshKeyPEM, ok := serverData["ssh_host_key"].(string); ok && sshKeyPEM != "" {
				fp, fpErr := sshPublicFingerprint([]byte(sshKeyPEM))
				if fpErr == nil {
					_, _ = fmt.Fprintf(w, "SSH host key fingerprint (from OpenBao): %s\n", fp)
				}
			}
		}
	}

	// -- SAN check: goproxy names required when goProxy is enabled --
	// Read server identity from OpenBao when not already loaded above.
	if serverData == nil && haveStore {
		path := defaultKVPrefix + "/data/identities/" + ns + "/server"
		result, readErr := store.readData(ctx, token, path)
		if readErr == nil && result != nil {
			serverData, _ = result["data"].(map[string]any)
			if metaMap, ok := result["metadata"].(map[string]any); ok {
				if v, ok := metaMap["version"].(float64); ok {
					serverVersion = int(v)
				}
			}
		}
	}

	// Determine which cert to inspect: OpenBao (preferred) or the running pod.
	var certPEM []byte
	if serverData != nil {
		if c, ok := serverData["tls.crt"].(string); ok && c != "" {
			certPEM = []byte(c)
		}
	}
	if certPEM == nil {
		podCert, podErr := readPodServerCert(ctx, *cfg, deps)
		if podErr == nil {
			certPEM = podCert
		}
	}
	if certPEM == nil {
		return nil // no cert to check — fresh install
	}

	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil
	}
	cert, parseErr := x509.ParseCertificate(block.Bytes)
	if parseErr != nil {
		return nil
	}

	oldFingerprint := certSHA256Fingerprint(cert.Raw)

	// Read live Helm values for goProxy and watchTunnel state.
	liveValues, liveErr := readLiveHelmValuesSubset(ctx, deps, ns)
	if liveErr != nil {
		return fmt.Errorf("read live Helm values for the identity preflight: %w", liveErr)
	}

	// Build the required name set. The only hard-required name is the URL
	// name that pipeline pods use: <fullname>-goproxy.<ns>.svc. When
	// rotation is needed, all four goproxy forms are added.
	const fullname = "oberth"
	urlName := fullname + "-goproxy." + ns + ".svc"
	var missingNames []string
	if liveValues.Argo.GoProxy.Enabled && !stringSliceContains(cert.DNSNames, urlName) {
		for _, name := range goProxyDNSNames(fullname, ns) {
			if !stringSliceContains(cert.DNSNames, name) {
				missingNames = append(missingNames, name)
			}
		}
	}
	if len(missingNames) == 0 {
		return nil // certificate covers all required names
	}

	// Missing goproxy names detected.
	if !haveStore || serverData == nil {
		return fmt.Errorf("the server certificate does not cover the goproxy service names (%s); "+
			"every Go pipeline step will fail after the upgrade. Re-run with --install-secretstore "+
			"and %s set so the installer can rotate the certificate",
			strings.Join(missingNames, ", "), baoTokenEnvVar)
	}

	if dryRun {
		_, _ = fmt.Fprintf(w, "  Identity preflight: server certificate lacks goproxy SANs: %s\n",
			strings.Join(missingNames, ", "))
		_, _ = fmt.Fprintf(w, "  Identity preflight: would rotate server certificate to add the missing names\n")
		if liveValues.WatchTunnel.Enabled && liveValues.WatchTunnel.OriginCACert != "" {
			_, _ = fmt.Fprintf(w, "  Identity preflight: would update watchTunnel.originCACert with the rotated certificate\n")
		}
		return nil
	}

	// -- Rotate the certificate: add the missing goproxy SANs --
	// Note: serverData["tls.key"] is a Go string returned by readData;
	// clearing a []byte(string) copy would be a no-op. Tracked as a
	// follow-up to review readData's return type.
	keyPEM, ok := serverData["tls.key"].(string)
	if !ok || keyPEM == "" {
		return errors.New("server identity in OpenBao is missing tls.key; cannot rotate certificate")
	}

	newCertPEM, rotateErr := rotateCertificate(cert, []byte(keyPEM), missingNames)
	if rotateErr != nil {
		return fmt.Errorf("rotate server certificate: %w", rotateErr)
	}

	// Write ONLY tls.crt back with CAS.
	writeOut, writeErr := store.authenticated(ctx, token, newCertPEM,
		"kv", "patch", "-mount="+defaultKVPrefix,
		fmt.Sprintf("-cas=%d", serverVersion),
		"identities/"+ns+"/server", "tls.crt=-")
	if writeErr != nil {
		return fmt.Errorf("write rotated certificate to OpenBao: %w\n%s", writeErr, writeOut)
	}

	// Read back and verify.
	if err := verifyRotation(ctx, store, token, ns, serverVersion, serverData, newCertPEM, cert, w); err != nil {
		return err
	}

	newBlock, _ := pem.Decode(newCertPEM)
	if newBlock == nil {
		return errors.New("rotated certificate PEM is invalid")
	}
	newCert, newParseErr := x509.ParseCertificate(newBlock.Bytes)
	if newParseErr != nil {
		return fmt.Errorf("parse rotated certificate: %w", newParseErr)
	}
	newFingerprint := certSHA256Fingerprint(newCert.Raw)
	_, _ = fmt.Fprintf(w, "Server certificate rotated: old=%s new=%s\n", oldFingerprint, newFingerprint)

	// Update watchTunnel.originCACert when the live value pins the old server leaf.
	if liveValues.WatchTunnel.Enabled && liveValues.WatchTunnel.OriginCACert != "" {
		originBlock, _ := pem.Decode([]byte(liveValues.WatchTunnel.OriginCACert))
		if originBlock != nil {
			originCert, originErr := x509.ParseCertificate(originBlock.Bytes)
			if originErr == nil {
				originFP := certSHA256Fingerprint(originCert.Raw)
				if originFP == oldFingerprint {
					cfg.watchTunnelOriginCACert = string(newCertPEM)
				} else {
					_, _ = fmt.Fprintf(w, "WARNING: watchTunnel.originCACert pins a different certificate "+
						"(fingerprint %s); the rotated server certificate has fingerprint %s. "+
						"Update the pin manually if needed.\n", originFP, newFingerprint)
				}
			}
		}
	}

	return nil
}

// readPodServerCert retrieves the server certificate from the running pod.
// Tries the legacy path (/etc/oberth/tls/tls.crt) first, then the OpenBao
// identity path (/run/oberth-identities/server/tls.crt).
func readPodServerCert(ctx context.Context, cfg Config, deps Deps) ([]byte, error) {
	if deps.RunCommand == nil {
		return nil, errors.New("no cluster command runner")
	}
	ns := cfg.Namespace
	if ns == "" {
		ns = DefaultNamespace
	}
	for _, path := range []string{"/etc/oberth/tls/tls.crt", "/run/oberth-identities/server/tls.crt"} {
		args := []string{}
		if deps.ContextName != "" {
			args = append(args, "--context", deps.ContextName)
		}
		args = append(args, "exec", "-n", ns, "deploy/oberth", "-c", "oberth", "--", "cat", path)
		cert, err := deps.RunCommand(ctx, nil, "kubectl", args...)
		if err == nil && len(cert) > 0 {
			return cert, nil
		}
	}
	return nil, errors.New("could not retrieve server certificate from the running pod")
}

// certSHA256Fingerprint returns the colon-separated SHA-256 fingerprint of
// a DER-encoded certificate (public data only).
func certSHA256Fingerprint(der []byte) string {
	h := sha256.Sum256(der)
	parts := make([]string, len(h))
	for i, b := range h {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

// parseCertPublicKey parses a PEM-encoded certificate and returns the
// PKIX-marshalled public key DER and the certificate's SHA-256 fingerprint.
// The caller uses the DER bytes for identity comparison and the fingerprint
// for human-readable error messages.
func parseCertPublicKey(certPEM []byte) (pubDER []byte, fingerprint string, err error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, "", errors.New("no PEM block found")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, "", err
	}
	pubDER, err = x509.MarshalPKIXPublicKey(cert.PublicKey)
	if err != nil {
		return nil, "", err
	}
	return pubDER, certSHA256Fingerprint(cert.Raw), nil
}

// sshPublicFingerprint parses an SSH private key PEM and returns the public
// key fingerprint (SHA256:...). The private key material is never logged.
func sshPublicFingerprint(privatePEM []byte) (string, error) {
	signer, err := ssh.ParsePrivateKey(privatePEM)
	if err != nil {
		return "", err
	}
	return ssh.FingerprintSHA256(signer.PublicKey()), nil
}

// rotateCertificate creates a new certificate that preserves the Subject,
// KeyUsage, ExtKeyUsage, NotAfter, and existing SANs of the original, adds
// the specified missing DNS names, and self-signs with the existing private
// key. The caller must clear keyPEM after this function returns.
func rotateCertificate(existing *x509.Certificate, keyPEM []byte, addNames []string) ([]byte, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("no PEM block in private key")
	}
	key, err := parseAnyPrivateKey(block.Bytes)
	clear(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}

	// Merge existing and new DNS names, deduplicating.
	allDNS := make([]string, 0, len(existing.DNSNames)+len(addNames))
	seen := make(map[string]bool, len(existing.DNSNames)+len(addNames))
	for _, name := range existing.DNSNames {
		if !seen[name] {
			allDNS = append(allDNS, name)
			seen[name] = true
		}
	}
	for _, name := range addNames {
		if !seen[name] {
			allDNS = append(allDNS, name)
			seen[name] = true
		}
	}

	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               existing.Subject,
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              existing.NotAfter,
		KeyUsage:              existing.KeyUsage,
		ExtKeyUsage:           existing.ExtKeyUsage,
		BasicConstraintsValid: true,
		IsCA:                  false,
		DNSNames:              allDNS,
		IPAddresses:           existing.IPAddresses,
	}

	pub := key.(crypto.Signer).Public()
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, key)
	if err != nil {
		return nil, fmt.Errorf("create certificate: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// parseAnyPrivateKey tries PKCS#8, PKCS#1 (RSA), and SEC 1 (EC) formats.
func parseAnyPrivateKey(der []byte) (crypto.PrivateKey, error) {
	if key, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return key, nil
	}
	if key, err := x509.ParseECPrivateKey(der); err == nil {
		return key, nil
	}
	return nil, errors.New("unable to parse private key in any known format")
}

// verifyRotation reads back the server identity from OpenBao after a CAS write
// and verifies the rotation preserved the private key and SSH host key.
func verifyRotation(ctx context.Context, store openBaoExec, token, ns string, oldVersion int, oldData map[string]any, newCertPEM []byte, oldCert *x509.Certificate, w io.Writer) error {
	path := defaultKVPrefix + "/data/identities/" + ns + "/server"
	readback, err := store.readData(ctx, token, path)
	if err != nil {
		return fmt.Errorf("readback after rotation: %w", err)
	}
	if readback == nil {
		return errors.New("server identity disappeared after rotation")
	}

	// Verify version incremented.
	metaMap, _ := readback["metadata"].(map[string]any)
	newVersionF, _ := metaMap["version"].(float64)
	if int(newVersionF) != oldVersion+1 {
		return fmt.Errorf("rotation version mismatch: expected %d, got %d", oldVersion+1, int(newVersionF))
	}

	dataMap, _ := readback["data"].(map[string]any)

	// Verify tls.key and ssh_host_key are unchanged (by SHA-256 hash).
	for _, field := range []string{"tls.key", "ssh_host_key"} {
		oldVal, _ := oldData[field].(string)
		newVal, _ := dataMap[field].(string)
		if sha256hex([]byte(oldVal)) != sha256hex([]byte(newVal)) {
			return fmt.Errorf("%s changed during rotation; refusing to proceed", field)
		}
	}

	// Verify the new cert parses and its public key equals the original.
	newCertBlock, _ := pem.Decode([]byte(dataMap["tls.crt"].(string)))
	if newCertBlock == nil {
		return errors.New("readback tls.crt is not valid PEM")
	}
	newCert, err := x509.ParseCertificate(newCertBlock.Bytes)
	if err != nil {
		return fmt.Errorf("readback tls.crt does not parse: %w", err)
	}

	oldPub, _ := x509.MarshalPKIXPublicKey(oldCert.PublicKey)
	newPub, _ := x509.MarshalPKIXPublicKey(newCert.PublicKey)
	if !bytesEqual(oldPub, newPub) {
		return errors.New("rotated certificate public key does not match the original")
	}

	return nil
}

func sha256hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func stringSliceContains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}
