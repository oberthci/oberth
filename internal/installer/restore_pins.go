package installer

// Restored secrets that pin the server they were backed up from (#074). A
// restore keeps the new install's SSH host key and CA, so a pipeline secret
// that pins the old host key (a known_hosts file for cloning through
// Oberth) fails with a key mismatch, and one that bundles the old CA fails
// TLS. The restore rewrites known_hosts lines naming only this server to its
// current host key, leaving every other line byte-identical, and names what
// it leaves for the operator: lines that also name other hosts, lines for
// loopback names (which mean a different machine elsewhere), lines pinning
// the old key under names it cannot attribute to this server, and secrets
// that hold the old CA.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1" // #nosec G505 -- OpenSSH hashed known_hosts entries are HMAC-SHA1 by definition.
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/secretstore"
)

// pinRewrite is one secret whose known_hosts lines name this server only.
type pinRewrite struct {
	Path string
	// Lines were rewritten to the current host key; Dropped duplicated a
	// line that already holds it. Hosts are the host specs rewritten.
	Lines, Dropped int
	Hosts          []string
}

// pinFinding is a secret with known_hosts lines left for the operator,
// with the host patterns concerned (public names; hashed ones as hashed).
type pinFinding struct {
	Path  string
	Hosts []string
}

func (f pinFinding) String() string {
	return f.Path + " (" + strings.Join(f.Hosts, ", ") + ")"
}

// serverPins is what the restore found in the archived secrets.
type serverPins struct {
	// Note says why SSH host-key pins were not checked ("" when they were).
	Note      string
	Rewritten []pinRewrite
	// Mixed lines list this server among other hosts; Loopback lines name
	// this server only by a loopback name; OldKey lines pin the backup's SSH
	// host key under names not attributed to this server. All are left.
	Mixed, Loopback, OldKey []pinFinding
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

// loopbackSpec reports whether a known_hosts host spec names a loopback
// host: that name reaches this server only from its own node, and a
// pipeline means another machine by it.
func loopbackSpec(spec string) bool {
	host := spec
	if strings.HasPrefix(spec, "[") {
		if end := strings.LastIndex(spec, "]"); end > 0 {
			host = spec[1:end]
		}
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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

// readServerIdentity reads the install's own server identity bundle; the
// caller clears it.
func readServerIdentity(ctx context.Context, bao openBaoExec, token, namespace string) (*identityBundle, error) {
	raw, err := bao.readRaw(ctx, token, defaultKVPrefix+"/data/"+installIdentityPrefix+namespace+"/server")
	if err != nil || raw == nil {
		return nil, err
	}
	defer clear(raw)
	return decodeIdentityBundle(raw)
}

var errNotOpenSSHKey = errors.New("not an OpenSSH private key file")

// openSSHPublicKey reads the public key an OpenSSH private key file
// (openssh-key-v1) carries in the clear ahead of its private section, which
// is never parsed.
func openSSHPublicKey(privatePEM []byte) (ssh.PublicKey, error) {
	block, _ := pem.Decode(privatePEM)
	if block == nil || block.Type != "OPENSSH PRIVATE KEY" {
		return nil, errNotOpenSSHKey
	}
	defer clear(block.Bytes)
	const magic = "openssh-key-v1\x00"
	rest, ok := bytes.CutPrefix(block.Bytes, []byte(magic))
	if !ok {
		return nil, errNotOpenSSHKey
	}
	next := func() ([]byte, bool) {
		if len(rest) < 4 {
			return nil, false
		}
		n := binary.BigEndian.Uint32(rest)
		if int64(n) > int64(len(rest)-4) {
			return nil, false
		}
		value := rest[4 : 4+n]
		rest = rest[4+n:]
		return value, true
	}
	for range 3 { // cipher name, KDF name, KDF options
		if _, ok := next(); !ok {
			return nil, errNotOpenSSHKey
		}
	}
	if len(rest) < 4 || binary.BigEndian.Uint32(rest) != 1 {
		return nil, errNotOpenSSHKey
	}
	rest = rest[4:]
	blob, ok := next()
	if !ok {
		return nil, errNotOpenSSHKey
	}
	return ssh.ParsePublicKey(bytes.Clone(blob))
}

// serverHostPublicKey returns the public half of the identity's SSH host
// key: from the key file's public section, or (other formats) by parsing
// the key in memory, which only the restore path does.
func serverHostPublicKey(bundle *identityBundle) (ssh.PublicKey, error) {
	privatePEM := bundle.field("ssh_host_key")
	if len(privatePEM) == 0 {
		return nil, nil
	}
	if key, err := openSSHPublicKey(privatePEM); err == nil {
		return key, nil
	}
	signer, err := ssh.ParsePrivateKey(privatePEM)
	if err != nil {
		return nil, fmt.Errorf("parse the server SSH host key: %w", err)
	}
	return signer.PublicKey(), nil
}

// recordServerIdentity notes the public parts of the source's server
// identity in the backup's reference: its certificate and SSH host public
// key, read from the key file's public section only (#074).
func recordServerIdentity(ctx context.Context, bao openBaoExec, token, namespace string, reference *openBaoReference) {
	bundle, err := readServerIdentity(ctx, bao, token, namespace)
	if err != nil || bundle == nil {
		return
	}
	defer bundle.clear()
	reference.ServerCertificate = string(bundle.field("tls.crt"))
	if key, err := openSSHPublicKey(bundle.field("ssh_host_key")); err == nil {
		reference.ServerSSHHostKeys = []string{strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))}
	}
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

// matchHosts returns this server's host specs a line's patterns name (a
// hashed |1|salt|hash pattern resolved to the spec it hashes) and how many
// patterns name anything else.
func matchHosts(patterns string, ours map[string]bool) (mine []string, others int) {
	for _, pattern := range strings.Split(patterns, ",") {
		switch {
		case strings.HasPrefix(pattern, "|1|"):
			if spec := hashedHostMatch(pattern, ours); spec != "" {
				mine = append(mine, spec)
			} else {
				others++
			}
		case ours[strings.ToLower(pattern)]:
			mine = append(mine, strings.ToLower(pattern))
		default:
			others++
		}
	}
	return mine, others
}

func hashedHostMatch(pattern string, ours map[string]bool) string {
	parts := strings.Split(strings.TrimPrefix(pattern, "|1|"), "|")
	if len(parts) != 2 {
		return ""
	}
	salt, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil {
		return ""
	}
	want, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	for _, host := range slices.Sorted(maps.Keys(ours)) {
		mac := hmac.New(sha1.New, salt)
		mac.Write([]byte(host))
		if hmac.Equal(mac.Sum(nil), want) {
			return host
		}
	}
	return ""
}

// knownHostsResult is what rewriteKnownHosts did to one value.
type knownHostsResult struct {
	out                      string
	rewritten, dropped       int
	rewrittenHosts           []string
	mixed, loopback, oldKeys []string
}

// rewriteKnownHosts rewrites the lines of value that name only this server,
// by names other than loopback ones, with another key to key, and drops a
// line that would then repeat one already holding it. Every other line
// stays byte-identical; lines left for the operator are reported.
func rewriteKnownHosts(value string, ours map[string]bool, key ssh.PublicKey, oldKeys [][]byte) knownHostsResult {
	keyBlob := key.Marshal()
	replacement := key.Type() + " " + base64.StdEncoding.EncodeToString(keyBlob)
	lines := strings.Split(value, "\n")
	kept := make([]string, 0, len(lines))
	current := map[string]bool{}
	var result knownHostsResult
	for _, line := range lines {
		entry, ok := parseKnownHostsLine(line)
		if !ok {
			kept = append(kept, line)
			continue
		}
		mine, others := matchHosts(entry.hosts, ours)
		switch {
		case len(mine) == 0:
			if slices.ContainsFunc(oldKeys, func(old []byte) bool { return bytes.Equal(old, entry.blob) }) {
				result.oldKeys = append(result.oldKeys, entry.hosts)
			}
			kept = append(kept, line)
		case others > 0 || slices.ContainsFunc(mine, loopbackSpec):
			// Left as is; worth naming only while it pins another key.
			if !bytes.Equal(entry.blob, keyBlob) {
				if others > 0 {
					result.mixed = append(result.mixed, entry.hosts)
				} else {
					result.loopback = append(result.loopback, mine...)
				}
			}
			kept = append(kept, line)
		case current[entry.hosts]:
			result.dropped++
		case bytes.Equal(entry.blob, keyBlob):
			current[entry.hosts] = true
			kept = append(kept, line)
		default:
			current[entry.hosts] = true
			result.rewritten++
			result.rewrittenHosts = append(result.rewrittenHosts, mine...)
			cr := ""
			if strings.HasSuffix(line, "\r") {
				cr = "\r"
			}
			kept = append(kept, entry.hosts+" "+replacement+entry.comment+cr)
		}
	}
	result.out = value
	if result.rewritten > 0 || result.dropped > 0 {
		result.out = strings.Join(kept, "\n")
	}
	return result
}

func sortedUnique(values []string) []string {
	values = slices.Clone(values)
	slices.Sort(values)
	return slices.Compact(values)
}

// rewriteHostKeyPins returns kv with known_hosts pins of this server updated
// to key. Secrets it does not change are shared with kv; changed ones get a
// copy of their data. oldKeys are the backup's SSH host public keys (wire
// form), reported where restore cannot attribute their names.
func rewriteHostKeyPins(kv kvExport, hosts []string, key ssh.PublicKey, oldKeys [][]byte) (kvExport, serverPins) {
	ours := map[string]bool{}
	for _, host := range hosts {
		ours[host] = true
	}
	out := kv
	out.Secrets = slices.Clone(kv.Secrets)
	var pins serverPins
	for i, secret := range out.Secrets {
		var changed map[string]any
		rewrite := pinRewrite{Path: secret.Path}
		var mixed, loopback, oldKey []string
		for _, field := range slices.Sorted(maps.Keys(secret.Data)) {
			value, ok := secret.Data[field].(string)
			if !ok {
				continue
			}
			result := rewriteKnownHosts(value, ours, key, oldKeys)
			mixed, loopback, oldKey = append(mixed, result.mixed...), append(loopback, result.loopback...), append(oldKey, result.oldKeys...)
			if result.out == value {
				continue
			}
			if changed == nil {
				changed = maps.Clone(secret.Data)
			}
			changed[field] = result.out
			rewrite.Lines += result.rewritten
			rewrite.Dropped += result.dropped
			rewrite.Hosts = append(rewrite.Hosts, result.rewrittenHosts...)
		}
		if changed != nil {
			out.Secrets[i].Data = changed
			rewrite.Hosts = sortedUnique(rewrite.Hosts)
			pins.Rewritten = append(pins.Rewritten, rewrite)
		}
		for _, found := range []struct {
			hosts []string
			into  *[]pinFinding
		}{{mixed, &pins.Mixed}, {loopback, &pins.Loopback}, {oldKey, &pins.OldKey}} {
			if len(found.hosts) > 0 {
				*found.into = append(*found.into, pinFinding{Path: secret.Path, Hosts: sortedUnique(found.hosts)})
			}
		}
	}
	return out, pins
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

// backupHostKeys are the backup's SSH host public keys in wire form.
func backupHostKeys(reference openBaoReference) [][]byte {
	var keys [][]byte
	for _, line := range reference.ServerSSHHostKeys {
		if key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line)); err == nil {
			keys = append(keys, key.Marshal())
		}
	}
	return keys
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
	var cert []byte
	var key ssh.PublicKey
	bundle, err := readServerIdentity(ctx, bao, token, namespace)
	if err == nil && bundle != nil {
		cert = bytes.Clone(bundle.field("tls.crt"))
		key, err = serverHostPublicKey(bundle)
		bundle.clear()
	}
	oldCA := secretsPinningCA(archive.KV, backupServerCAs(archive.Reference), targetServerCAs(ctx, bao, token, cert))
	note := ""
	switch {
	case err != nil:
		note = err.Error()
	case key == nil:
		note = "this install has no SSH host key in OpenBao (" + installIdentityPrefix + namespace + "/server)"
	}
	var hosts []string
	if note == "" {
		if hosts, err = serverSSHHosts(ctx, deps, namespace, server); err != nil {
			note = err.Error()
		}
	}
	if note != "" {
		return archive.KV, serverPins{Note: note, OldCA: oldCA}
	}
	kv, pins := rewriteHostKeyPins(archive.KV, hosts, key, backupHostKeys(archive.Reference))
	pins.OldCA = oldCA
	return kv, pins
}
