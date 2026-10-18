package installer

// Restored secrets that pin the server they were backed up from (#074). A
// restore keeps the new install's SSH host key and CA, so a pipeline secret
// that pins the old host key (a known_hosts file for cloning through
// Oberth) fails with a key mismatch, and one that bundles the old CA fails
// TLS. The restore rewrites known_hosts lines naming this server to its
// current host key, leaving every other line byte-identical, and names the
// secrets that hold the old CA for the operator to update.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1" // #nosec G505 -- OpenSSH hashed known_hosts entries are HMAC-SHA1 by definition.
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/secretstore"
)

// pinRewrite is one secret whose known_hosts lines were updated.
type pinRewrite struct {
	Path string
	// Lines were rewritten to the current host key; Dropped duplicated a
	// line that already holds it.
	Lines, Dropped int
}

// serverPins is what the restore found in the archived secrets.
type serverPins struct {
	// Note says why SSH host-key pins were not checked ("" when they were).
	Note      string
	Rewritten []pinRewrite
	// Mixed name secrets with a line that lists this server among other
	// hosts; it is left as it is.
	Mixed []string
	// OldCA name secrets holding the backup's server CA, which this install
	// replaced.
	OldCA []string
}

// sshHostSpec spells a host the way known_hosts does.
func sshHostSpec(host string, port int32) string {
	if port == 22 {
		return strings.ToLower(host)
	}
	return "[" + strings.ToLower(host) + "]:" + strconv.Itoa(int(port))
}

// serverSSHHosts lists the names this install's SSH endpoint is reached by:
// its Service DNS names at the service port, and the names and addresses
// its server certificate covers (OBERTH_TLS_NAMES) at the node port.
func serverSSHHosts(ctx context.Context, deps Deps, namespace string, server serverDeployment) ([]string, error) {
	service, err := deps.KubeClient.CoreV1().Services(namespace).Get(ctx, "oberth", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read Service %s/oberth: %w", namespace, err)
	}
	var port, nodePort int32
	for _, p := range service.Spec.Ports {
		if p.Name == "ssh" {
			port, nodePort = p.Port, p.NodePort
		}
	}
	if port == 0 {
		return nil, fmt.Errorf("service %s/oberth has no ssh port", namespace)
	}
	dnsNames, _ := secretstore.ServerTLSNames(namespace, nil)
	var hosts []string
	for _, name := range dnsNames {
		if name == "oberth" || strings.HasPrefix(name, "oberth.") {
			hosts = append(hosts, sshHostSpec(name, port))
		}
	}
	if nodePort > 0 {
		for _, container := range server.PodSpec.Containers {
			for _, env := range container.Env {
				if env.Name != "OBERTH_TLS_NAMES" {
					continue
				}
				for _, name := range strings.Split(env.Value, ",") {
					if name = strings.TrimSpace(name); name != "" {
						hosts = append(hosts, sshHostSpec(name, nodePort))
					}
				}
			}
		}
	}
	slices.Sort(hosts)
	return slices.Compact(hosts), nil
}

// serverIdentityPublic reads the public parts of the install's own server
// identity: its SSH host key and server certificate. The private key is
// parsed in memory and cleared; only public values are returned.
func serverIdentityPublic(ctx context.Context, bao openBaoExec, token, namespace string) (ssh.PublicKey, []byte, error) {
	raw, err := bao.readRaw(ctx, token, defaultKVPrefix+"/data/"+installIdentityPrefix+namespace+"/server")
	if err != nil || raw == nil {
		return nil, nil, err
	}
	bundle, err := decodeIdentityBundle(raw)
	clear(raw)
	if err != nil {
		return nil, nil, err
	}
	defer bundle.clear()
	cert := bytes.Clone(bundle.field("tls.crt"))
	privatePEM := bundle.field("ssh_host_key")
	if len(privatePEM) == 0 {
		return nil, cert, nil
	}
	signer, err := ssh.ParsePrivateKey(privatePEM)
	if err != nil {
		return nil, cert, fmt.Errorf("parse the server SSH host key: %w", err)
	}
	return signer.PublicKey(), cert, nil
}

// knownHostsEntry is one parsed known_hosts line.
type knownHostsEntry struct {
	hosts   string
	blob    []byte
	comment string // everything after the key, with its leading space
}

func parseKnownHostsLine(line string) (knownHostsEntry, bool) {
	trimmed := strings.TrimRight(line, "\r")
	fields := strings.Fields(trimmed)
	if len(fields) < 3 || strings.HasPrefix(fields[0], "#") || strings.HasPrefix(fields[0], "@") {
		return knownHostsEntry{}, false
	}
	blob, err := base64.StdEncoding.DecodeString(fields[2])
	if err != nil {
		return knownHostsEntry{}, false
	}
	key, err := ssh.ParsePublicKey(blob)
	if err != nil || key.Type() != fields[1] {
		return knownHostsEntry{}, false
	}
	end := strings.Index(trimmed, fields[2]) + len(fields[2])
	return knownHostsEntry{hosts: fields[0], blob: blob, comment: trimmed[end:]}, true
}

// matchHosts counts the host patterns of a line that name this server and
// those that name anything else. Hashed (|1|salt|hash) patterns are tested
// against every name of this server.
func matchHosts(patterns string, ours map[string]bool) (mine, others int) {
	for _, pattern := range strings.Split(patterns, ",") {
		switch {
		case strings.HasPrefix(pattern, "|1|"):
			if hashedHostMatches(pattern, ours) {
				mine++
			} else {
				others++
			}
		case ours[strings.ToLower(pattern)]:
			mine++
		default:
			others++
		}
	}
	return mine, others
}

func hashedHostMatches(pattern string, ours map[string]bool) bool {
	parts := strings.Split(strings.TrimPrefix(pattern, "|1|"), "|")
	if len(parts) != 2 {
		return false
	}
	salt, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	want, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	for host := range ours {
		mac := hmac.New(sha1.New, salt)
		mac.Write([]byte(host))
		if hmac.Equal(mac.Sum(nil), want) {
			return true
		}
	}
	return false
}

// rewriteKnownHosts rewrites the lines of value that name only this server
// with another key to key, and drops a line that would then repeat one
// already holding it. Every other line stays byte-identical.
func rewriteKnownHosts(value string, ours map[string]bool, key ssh.PublicKey) (out string, rewritten, dropped int, mixed bool) {
	keyBlob := key.Marshal()
	replacement := key.Type() + " " + base64.StdEncoding.EncodeToString(keyBlob)
	lines := strings.Split(value, "\n")
	kept := make([]string, 0, len(lines))
	current := map[string]bool{}
	for _, line := range lines {
		entry, ok := parseKnownHostsLine(line)
		if !ok {
			kept = append(kept, line)
			continue
		}
		mine, others := matchHosts(entry.hosts, ours)
		switch {
		case mine == 0:
			kept = append(kept, line)
		case others > 0:
			mixed = true
			kept = append(kept, line)
		case current[entry.hosts]:
			dropped++
		case bytes.Equal(entry.blob, keyBlob):
			current[entry.hosts] = true
			kept = append(kept, line)
		default:
			current[entry.hosts] = true
			rewritten++
			cr := ""
			if strings.HasSuffix(line, "\r") {
				cr = "\r"
			}
			kept = append(kept, entry.hosts+" "+replacement+entry.comment+cr)
		}
	}
	if rewritten == 0 && dropped == 0 {
		return value, 0, 0, mixed
	}
	return strings.Join(kept, "\n"), rewritten, dropped, mixed
}

// rewriteHostKeyPins returns kv with known_hosts pins of this server updated
// to key. Secrets it does not change are shared with kv; changed ones get a
// copy of their data.
func rewriteHostKeyPins(kv kvExport, hosts []string, key ssh.PublicKey) (kvExport, []pinRewrite, []string) {
	ours := map[string]bool{}
	for _, host := range hosts {
		ours[host] = true
	}
	out := kv
	out.Secrets = slices.Clone(kv.Secrets)
	var rewrites []pinRewrite
	var mixed []string
	for i, secret := range out.Secrets {
		fields := slices.Sorted(maps.Keys(secret.Data))
		var changed map[string]any
		rewrite := pinRewrite{Path: secret.Path}
		secretMixed := false
		for _, field := range fields {
			value, ok := secret.Data[field].(string)
			if !ok {
				continue
			}
			updated, lines, dropped, isMixed := rewriteKnownHosts(value, ours, key)
			secretMixed = secretMixed || isMixed
			if updated == value {
				continue
			}
			if changed == nil {
				changed = maps.Clone(secret.Data)
			}
			changed[field] = updated
			rewrite.Lines += lines
			rewrite.Dropped += dropped
		}
		if changed != nil {
			out.Secrets[i].Data = changed
			rewrites = append(rewrites, rewrite)
		}
		if secretMixed {
			mixed = append(mixed, secret.Path)
		}
	}
	return out, rewrites, mixed
}

// certificateFingerprints returns the SHA-256 of every PEM certificate in
// text.
func certificateFingerprints(text string) []string {
	var fingerprints []string
	rest := []byte(text)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return fingerprints
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			continue
		}
		sum := sha256.Sum256(block.Bytes)
		fingerprints = append(fingerprints, hex.EncodeToString(sum[:]))
	}
}

// backupServerCAs are the fingerprints of the server CA the backup's clients
// pinned: its PKI CA and its server certificate, as the reference records.
func backupServerCAs(reference openBaoReference) map[string]bool {
	fingerprints := map[string]bool{}
	pems := slices.Collect(maps.Values(reference.PKICACerts))
	pems = append(pems, reference.ServerCertificate)
	for _, text := range pems {
		for _, fingerprint := range certificateFingerprints(text) {
			fingerprints[fingerprint] = true
		}
	}
	return fingerprints
}

// targetServerCAs are the same fingerprints for this install.
func targetServerCAs(ctx context.Context, bao openBaoExec, token string, serverCert []byte) map[string]bool {
	fingerprints := map[string]bool{}
	for _, fingerprint := range certificateFingerprints(string(serverCert)) {
		fingerprints[fingerprint] = true
	}
	mounts, err := bao.listMounts(ctx, token, "secrets")
	if err != nil {
		return fingerprints
	}
	for path, mount := range mounts {
		if mount.Type != "pki" {
			continue
		}
		ca, err := bao.readData(ctx, token, strings.TrimSuffix(path, "/")+"/cert/ca")
		if err != nil {
			continue
		}
		if certificate, ok := ca["certificate"].(string); ok {
			for _, fingerprint := range certificateFingerprints(certificate) {
				fingerprints[fingerprint] = true
			}
		}
	}
	return fingerprints
}

// secretsPinningCA names the secrets holding one of the old CAs that this
// install does not also use.
func secretsPinningCA(kv kvExport, old, current map[string]bool) []string {
	stale := map[string]bool{}
	for fingerprint := range old {
		if !current[fingerprint] {
			stale[fingerprint] = true
		}
	}
	if len(stale) == 0 {
		return nil
	}
	var paths []string
	for _, secret := range kv.Secrets {
		for _, value := range secret.Data {
			text, ok := value.(string)
			if !ok {
				continue
			}
			if slices.ContainsFunc(certificateFingerprints(text), func(fingerprint string) bool { return stale[fingerprint] }) {
				paths = append(paths, secret.Path)
				break
			}
		}
	}
	sort.Strings(paths)
	return paths
}

// checkServerPins finds the archived secrets that pin the backup's server
// and returns kv with this server's known_hosts pins updated.
func checkServerPins(ctx context.Context, deps Deps, namespace string, server serverDeployment, bao openBaoExec, token string, archive restoreArchive) (kvExport, serverPins) {
	var pins serverPins
	key, cert, err := serverIdentityPublic(ctx, bao, token, namespace)
	pins.OldCA = secretsPinningCA(archive.KV, backupServerCAs(archive.Reference), targetServerCAs(ctx, bao, token, cert))
	switch {
	case err != nil:
		pins.Note = err.Error()
		return archive.KV, pins
	case key == nil:
		pins.Note = "this install has no SSH host key in OpenBao (" + installIdentityPrefix + namespace + "/server)"
		return archive.KV, pins
	}
	hosts, err := serverSSHHosts(ctx, deps, namespace, server)
	if err != nil {
		pins.Note = err.Error()
		return archive.KV, pins
	}
	kv, rewrites, mixed := rewriteHostKeyPins(archive.KV, hosts, key)
	pins.Rewritten, pins.Mixed = rewrites, mixed
	return kv, pins
}
