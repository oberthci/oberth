package installer

// Restored secrets that pin the backup's server (#074).

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- OpenSSH hashed known_hosts entries are HMAC-SHA1.
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func newEd25519HostKey(t *testing.T) (ssh.PublicKey, []byte) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return key, pem.EncodeToMemory(block)
}

func newECDSAHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(&private.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func testCertificatePEM(t *testing.T, name string) string {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &private.PublicKey, private)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func knownHostsLine(hosts string, key ssh.PublicKey, comment string) string {
	line := hosts + " " + key.Type() + " " + base64.StdEncoding.EncodeToString(key.Marshal())
	if comment != "" {
		line += " " + comment
	}
	return line
}

func hashedHost(host string) string {
	salt := []byte("0123456789abcdefghij")
	mac := hmac.New(sha1.New, salt)
	mac.Write([]byte(host))
	return "|1|" + base64.StdEncoding.EncodeToString(salt) + "|" + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func TestRewriteKnownHostsTouchesOnlyThisServer(t *testing.T) {
	t.Parallel()
	current, _ := newEd25519HostKey(t)
	old, older, github := newECDSAHostKey(t), newECDSAHostKey(t), newECDSAHostKey(t)
	ours := map[string]bool{}
	for _, spec := range []string{"[oberth.oberth.svc.cluster.local]:2222", "[oberth.oberth.svc]:2222", "[tuxbox]:30022",
		"oberth.example", "[2001:db8::5]:30022", "[::1]:30022", "[localhost]:30022", "[127.0.0.1]:30022"} {
		ours[spec] = true
	}
	untouched := []string{
		"# pinned hosts",
		knownHostsLine("github.com", github, ""),
		knownHostsLine("[elsewhere.example]:2222", older, ""),
		"@cert-authority " + knownHostsLine("[oberth.oberth.svc.cluster.local]:2222", old, ""),
		"@revoked " + knownHostsLine("[tuxbox]:30022", old, ""),
		knownHostsLine("[oberth.example]:2200", old, ""), // another port: not this server
	}
	mixed := knownHostsLine("[oberth.oberth.svc.cluster.local]:2222,gitlab.com", old, "")
	loopback := []string{
		knownHostsLine("[::1]:30022", old, ""),
		knownHostsLine("[localhost]:30022,[127.0.0.1]:30022", old, ""),
	}
	elsewhere := knownHostsLine("[tuxbox.lan]:30022", old, "")
	value := strings.Join(append(append(append([]string{}, untouched...), mixed, elsewhere), append(loopback,
		knownHostsLine("[oberth.oberth.svc.cluster.local]:2222", old, "pins-comment-canary"),
		knownHostsLine(hashedHost("[oberth.oberth.svc]:2222"), old, "")+"\r",
		knownHostsLine("[tuxbox]:30022", old, ""),
		knownHostsLine("[tuxbox]:30022", older, ""),
		knownHostsLine("oberth.example", old, ""),
		knownHostsLine("[2001:db8::5]:30022", old, ""),
		"",
	)...), "\n")
	result := rewriteKnownHosts(value, ours, current, [][]byte{old.Marshal()})
	if result.rewritten != 5 || result.dropped != 1 {
		t.Fatalf("rewritten %d dropped %d, want 5 and 1:\n%s", result.rewritten, result.dropped, result.out)
	}
	want := strings.Join(append(append(append([]string{}, untouched...), mixed, elsewhere), append(loopback,
		knownHostsLine("[oberth.oberth.svc.cluster.local]:2222", current, "pins-comment-canary"),
		knownHostsLine(hashedHost("[oberth.oberth.svc]:2222"), current, "")+"\r",
		knownHostsLine("[tuxbox]:30022", current, ""),
		knownHostsLine("oberth.example", current, ""),
		knownHostsLine("[2001:db8::5]:30022", current, ""),
		"",
	)...), "\n")
	if result.out != want {
		t.Fatalf("rewritten known_hosts:\n%s\nwant:\n%s", result.out, want)
	}
	if got := sortedUnique(result.rewrittenHosts); strings.Join(got, " ") != "[2001:db8::5]:30022 [oberth.oberth.svc.cluster.local]:2222 [oberth.oberth.svc]:2222 [tuxbox]:30022 oberth.example" {
		t.Fatalf("rewritten hosts %v", got)
	}
	if strings.Join(sortedUnique(result.loopback), " ") != "[127.0.0.1]:30022 [::1]:30022 [localhost]:30022" ||
		len(result.mixed) != 1 || strings.Join(result.oldKeys, " ") != "[oberth.example]:2200 [tuxbox.lan]:30022" {
		t.Fatalf("reported loopback %v mixed %v old key %v", result.loopback, result.mixed, result.oldKeys)
	}
	// Already current: nothing changes, byte for byte.
	again := rewriteKnownHosts(result.out, ours, current, nil)
	if again.out != result.out || again.rewritten != 0 || again.dropped != 0 {
		t.Fatalf("a current known_hosts was changed again: %d rewritten, %d dropped", again.rewritten, again.dropped)
	}
	// Text that is not known_hosts is left alone.
	for _, text := range []string{"", "plain secret value", "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----\n"} {
		if got := rewriteKnownHosts(text, ours, current, nil); got.out != text || got.rewritten != 0 {
			t.Fatalf("rewrote a non-known_hosts value %q", text)
		}
	}
}

func TestSSHHostSpecsAndLoopback(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		host     string
		port     int32
		spec     string
		loopback bool
	}{
		{"Oberth.Example", 22, "oberth.example", false},
		{"tuxbox", 30022, "[tuxbox]:30022", false},
		{"2001:db8::5", 30022, "[2001:db8::5]:30022", false},
		{"::1", 30022, "[::1]:30022", true},
		{"127.0.0.2", 22, "127.0.0.2", true},
		{"localhost", 30022, "[localhost]:30022", true},
		{"app.localhost", 22, "app.localhost", true},
	} {
		spec := sshHostSpec(tc.host, tc.port)
		if spec != tc.spec || loopbackSpec(spec) != tc.loopback {
			t.Errorf("%s:%d -> %q loopback %v, want %q %v", tc.host, tc.port, spec, loopbackSpec(spec), tc.spec, tc.loopback)
		}
	}
}

// The backup records the server's SSH host public key from the key file's
// public section; a key in another format is not parsed at backup time.
func TestOpenSSHPublicKeyReadsOnlyThePublicSection(t *testing.T) {
	t.Parallel()
	key, privatePEM := newEd25519HostKey(t)
	got, err := openSSHPublicKey(privatePEM)
	if err != nil || !bytes.Equal(got.Marshal(), key.Marshal()) {
		t.Fatalf("public key from the OpenSSH file: %v", err)
	}
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openSSHPublicKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); err == nil {
		t.Fatal("a PKCS#8 key was parsed as an OpenSSH file")
	}
	for _, broken := range [][]byte{nil, []byte("junk"), pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: []byte("openssh-key-v1\x00\x00\x00\xff")})} {
		if _, err := openSSHPublicKey(broken); err == nil {
			t.Fatalf("a malformed key file was accepted: %q", broken)
		}
	}
}

// pinsFixture backs up a source whose pipeline secrets pin its SSH host key
// and server CA, and returns a reinstalled target with a new host key; with
// samePin the archive already pins the target's key.
func pinsFixture(t *testing.T, sourceCA, targetCA string, samePin bool) (string, *fakeBackupCluster, ssh.PublicKey) {
	t.Helper()
	hostKey, hostKeyPEM := newEd25519HostKey(t)
	sourceKey, sourceKeyPEM := newEd25519HostKey(t)
	pinnedKey := sourceKey
	if samePin {
		pinnedKey = hostKey
	}
	archiveBytes, raw, runID := sourceDatabase(t)
	source := newFakeBackupCluster(t, sourceCompat, grantsConfigMap(testGrants))
	source.database, source.rawFiles = archiveBytes, raw
	source.clusterUID = "source-cluster"
	seedSourceOpenBao(source.bao)
	source.bao.pkiCA = sourceCA
	source.bao.kv["identities/oberth/server"] = map[string]any{"ssh_host_key": string(sourceKeyPEM), "tls.crt": testCertificatePEM(t, "source-server")}
	github := newECDSAHostKey(t)
	source.bao.kv["release/git-known-hosts"] = map[string]any{"known_hosts": strings.Join([]string{
		knownHostsLine("github.com", github, ""),
		knownHostsLine("[oberth.oberth.svc.cluster.local]:2222", pinnedKey, "pins-comment-canary"),
		knownHostsLine("[tuxbox]:30022", pinnedKey, ""),
		knownHostsLine("[localhost]:30022", pinnedKey, ""),
		knownHostsLine("[tuxbox.lan]:30022", pinnedKey, ""),
		"",
	}, "\n")}
	source.bao.kv["release/ca-bundle"] = map[string]any{"ca.crt": sourceCA, "token": "pins-token-canary"}
	output := filepath.Join(t.TempDir(), "state.cms")
	var out bytes.Buffer
	cfg := BackupConfig{Output: output, Recipient: "recipient.pem", ToolVersion: "v0.17.37"}
	cfg.applyDefaults()
	if err := runBackup(context.Background(), cfg, source.deps(&out)); err != nil {
		t.Fatalf("backup: %v\n%s", err, out.String())
	}

	target := restoreTargetCluster(t, runID)
	target.clusterUID = "target-cluster"
	target.bao.kv["identities/oberth/server"] = map[string]any{"ssh_host_key": string(hostKeyPEM), "tls.crt": testCertificatePEM(t, "target-server")}
	target.bao.pkiCA = targetCA
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "oberth", Namespace: DefaultNamespace},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort, Ports: []corev1.ServicePort{
			{Name: "ssh", Port: 2222, NodePort: 30022}, {Name: "https", Port: 8443, NodePort: 30443},
		}},
	}
	if err := target.client.Tracker().Add(service); err != nil {
		t.Fatal(err)
	}
	deployments := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	object, err := target.client.Tracker().Get(deployments, DefaultNamespace, "oberth")
	if err != nil {
		t.Fatal(err)
	}
	deployment := object.(*appsv1.Deployment).DeepCopy()
	container := &deployment.Spec.Template.Spec.Containers[0]
	container.Env = append(container.Env, corev1.EnvVar{Name: "OBERTH_TLS_NAMES", Value: "tuxbox,localhost,10.0.0.5"})
	if err := target.client.Tracker().Update(deployments, deployment, DefaultNamespace); err != nil {
		t.Fatal(err)
	}
	return output, target, hostKey
}

func TestBackupRecordsTheServerPublicIdentity(t *testing.T) {
	t.Parallel()
	ca := testCertificatePEM(t, "ca")
	archive, _, _ := pinsFixture(t, ca, ca, false)
	work := t.TempDir()
	if _, err := openBackupArchive(context.Background(), archive, fakeEnvelopeCipher{}, work); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(work, filepath.FromSlash(backupMemberReference)))
	if err != nil {
		t.Fatal(err)
	}
	var reference openBaoReference
	if err := json.Unmarshal(body, &reference); err != nil {
		t.Fatal(err)
	}
	if len(reference.ServerSSHHostKeys) != 1 || !strings.HasPrefix(reference.ServerSSHHostKeys[0], "ssh-ed25519 ") ||
		!strings.Contains(reference.ServerCertificate, "BEGIN CERTIFICATE") || bytes.Contains(body, []byte("PRIVATE KEY")) {
		t.Fatalf("reference records %q / certificate %v", reference.ServerSSHHostKeys, reference.ServerCertificate != "")
	}
}

func TestRestoreUpdatesSecretsPinningTheOldHostKey(t *testing.T) {
	t.Parallel()
	oldCA, newCA := testCertificatePEM(t, "source-ca"), testCertificatePEM(t, "target-ca")
	archive, target, hostKey := pinsFixture(t, oldCA, newCA, false)

	plan := runFixtureRestore(t, archive, target, RestoreConfig{DryRun: true})
	for _, want := range []string{
		"known_hosts pins of this server updated to its SSH host key: release/git-known-hosts (2 lines: [oberth.oberth.svc.cluster.local]:2222, [tuxbox]:30022)",
		"known_hosts lines for loopback names of this server, left as they are: release/git-known-hosts ([localhost]:30022)",
		"known_hosts lines pinning the backup's SSH host key under other names, left as they are: release/git-known-hosts ([tuxbox.lan]:30022)",
		"still hold the backup's server CA (update them by hand): release/ca-bundle",
	} {
		if !strings.Contains(plan, want) {
			t.Fatalf("dry-run plan lacks %q:\n%s", want, plan)
		}
	}
	if _, written := target.bao.kv["release/git-known-hosts"]; written {
		t.Fatal("the dry run wrote a secret")
	}

	out := runFixtureRestore(t, archive, target, RestoreConfig{})
	restored, _ := target.bao.kv["release/git-known-hosts"]["known_hosts"].(string)
	lines := strings.Split(restored, "\n")
	if len(lines) != 6 || !strings.HasPrefix(lines[0], "github.com ") ||
		lines[1] != knownHostsLine("[oberth.oberth.svc.cluster.local]:2222", hostKey, "pins-comment-canary") ||
		lines[2] != knownHostsLine("[tuxbox]:30022", hostKey, "") ||
		strings.Contains(lines[3], base64.StdEncoding.EncodeToString(hostKey.Marshal())) {
		t.Fatalf("restored known_hosts:\n%s", restored)
	}
	for _, want := range []string{
		"known_hosts pins of this server updated to its SSH host key in release/git-known-hosts (2 lines",
		"still hold the backup's server CA", "release/ca-bundle",
		"pin a loopback name of this server", "pin the backup's SSH host key under names this restore cannot attribute",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("restore output lacks %q:\n%s", want, out)
		}
	}
	// Planned again, the pins are already current and the plan says so.
	again := runFixtureRestore(t, archive, target, RestoreConfig{DryRun: true})
	if !strings.Contains(again, "known_hosts pins of this server already current here: release/git-known-hosts (2 lines") ||
		strings.Contains(again, "updated to its SSH host key:") {
		t.Fatalf("second restore output:\n%s", again)
	}
	for _, canary := range []string{"pins-comment-canary", "pins-token-canary", "BEGIN CERTIFICATE", "BEGIN OPENSSH"} {
		if strings.Contains(plan+out+again, canary) {
			t.Fatalf("restore output carries secret material %q", canary)
		}
	}
}

// A backup that already pins this install's host key and a CA this install
// also uses changes nothing and reports nothing.
func TestRestoreLeavesCurrentPinsAlone(t *testing.T) {
	t.Parallel()
	ca := testCertificatePEM(t, "shared-ca")
	archive, target, _ := pinsFixture(t, ca, ca, true)
	out := runFixtureRestore(t, archive, target, RestoreConfig{})
	if strings.Contains(out, "pins of this server updated") || strings.Contains(out, "pins of this server already current") || strings.Contains(out, "backup's server CA") || strings.Contains(out, "left as they") {
		t.Fatalf("current pins were reported:\n%s", out)
	}
}

// Without the server's Service the names are unknown: the restore says so
// and writes the secrets as archived.
func TestRestoreReportsUncheckedPins(t *testing.T) {
	t.Parallel()
	ca := testCertificatePEM(t, "ca")
	archive, target, _ := pinsFixture(t, ca, ca, false)
	if err := target.client.CoreV1().Services(DefaultNamespace).Delete(context.Background(), "oberth", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	out := runFixtureRestore(t, archive, target, RestoreConfig{DryRun: true})
	if !strings.Contains(out, "known_hosts pins of this server: read Service oberth/oberth") {
		t.Fatalf("unchecked pins not reported:\n%s", out)
	}
}

func TestSubjectKeyIdentifierRecipientLabel(t *testing.T) {
	t.Parallel()
	if got := (BackupRecipient{SubjectKeyID: "0A1B"}).String(); got != "subject key identifier 0A1B" {
		t.Fatalf("label %q", got)
	}
}
