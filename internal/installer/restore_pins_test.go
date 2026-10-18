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
	"encoding/pem"
	"math/big"
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
	ours := map[string]bool{"[oberth.oberth.svc.cluster.local]:2222": true, "[oberth.oberth.svc]:2222": true, "[tuxbox]:30022": true}
	githubLine := knownHostsLine("github.com", github, "")
	elsewhere := knownHostsLine("[elsewhere.example]:2222", old, "")
	mixed := knownHostsLine("[oberth.oberth.svc.cluster.local]:2222,gitlab.com", old, "")
	value := strings.Join([]string{
		"# pinned hosts",
		githubLine,
		knownHostsLine("[oberth.oberth.svc.cluster.local]:2222", old, "pins-comment-canary"),
		knownHostsLine(hashedHost("[oberth.oberth.svc]:2222"), old, "") + "\r",
		knownHostsLine("[tuxbox]:30022", old, ""),
		knownHostsLine("[tuxbox]:30022", older, ""),
		elsewhere,
		mixed,
		"",
	}, "\n")
	out, rewritten, dropped, isMixed := rewriteKnownHosts(value, ours, current)
	if rewritten != 3 || dropped != 1 || !isMixed {
		t.Fatalf("rewritten %d dropped %d mixed %v, want 3, 1, true:\n%s", rewritten, dropped, isMixed, out)
	}
	lines := strings.Split(out, "\n")
	want := []string{
		"# pinned hosts",
		githubLine,
		knownHostsLine("[oberth.oberth.svc.cluster.local]:2222", current, "pins-comment-canary"),
		knownHostsLine(hashedHost("[oberth.oberth.svc]:2222"), current, "") + "\r",
		knownHostsLine("[tuxbox]:30022", current, ""),
		elsewhere,
		mixed,
		"",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("rewritten known_hosts:\n%q\nwant:\n%q", lines, want)
	}
	// Already current: nothing changes, byte for byte.
	again, rewritten, dropped, _ := rewriteKnownHosts(out, ours, current)
	if again != out || rewritten != 0 || dropped != 0 {
		t.Fatalf("a current known_hosts was changed again: %d rewritten, %d dropped", rewritten, dropped)
	}
	// Text that is not known_hosts is left alone.
	for _, text := range []string{"", "plain secret value", "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----\n"} {
		if got, n, _, _ := rewriteKnownHosts(text, ours, current); got != text || n != 0 {
			t.Fatalf("rewrote a non-known_hosts value %q", text)
		}
	}
}

// pinsFixture backs up a source whose pipeline secrets pin its SSH host key
// and server CA, and returns a reinstalled target with a new host key; with
// samePin the archive already pins the target's key.
func pinsFixture(t *testing.T, sourceCA, targetCA string, samePin bool) (string, *fakeBackupCluster, ssh.PublicKey) {
	t.Helper()
	hostKey, hostKeyPEM := newEd25519HostKey(t)
	pinnedKey := newECDSAHostKey(t)
	if samePin {
		pinnedKey = hostKey
	}
	archiveBytes, raw, runID := sourceDatabase(t)
	source := newFakeBackupCluster(t, sourceCompat, grantsConfigMap(testGrants))
	source.database, source.rawFiles = archiveBytes, raw
	source.clusterUID = "source-cluster"
	seedSourceOpenBao(source.bao)
	source.bao.pkiCA = sourceCA
	github := newECDSAHostKey(t)
	source.bao.kv["release/git-known-hosts"] = map[string]any{"known_hosts": strings.Join([]string{
		knownHostsLine("github.com", github, ""),
		knownHostsLine("[oberth.oberth.svc.cluster.local]:2222", pinnedKey, "pins-comment-canary"),
		knownHostsLine("[tuxbox]:30022", pinnedKey, ""),
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
	container.Env = append(container.Env, corev1.EnvVar{Name: "OBERTH_TLS_NAMES", Value: "tuxbox,10.0.0.5"})
	if err := target.client.Tracker().Update(deployments, deployment, DefaultNamespace); err != nil {
		t.Fatal(err)
	}
	return output, target, hostKey
}

func TestRestoreUpdatesSecretsPinningTheOldHostKey(t *testing.T) {
	t.Parallel()
	oldCA, newCA := testCertificatePEM(t, "source-ca"), testCertificatePEM(t, "target-ca")
	archive, target, hostKey := pinsFixture(t, oldCA, newCA, false)

	plan := runFixtureRestore(t, archive, target, RestoreConfig{DryRun: true})
	for _, want := range []string{
		"known_hosts pins of this server updated to its SSH host key: release/git-known-hosts (2 lines)",
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
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "github.com ") ||
		lines[1] != knownHostsLine("[oberth.oberth.svc.cluster.local]:2222", hostKey, "pins-comment-canary") ||
		lines[2] != knownHostsLine("[tuxbox]:30022", hostKey, "") {
		t.Fatalf("restored known_hosts:\n%s", restored)
	}
	if !strings.Contains(out, "known_hosts pins of this server updated to its SSH host key in release/git-known-hosts (2 lines)") ||
		!strings.Contains(out, "still hold the backup's server CA") || !strings.Contains(out, "release/ca-bundle") {
		t.Fatalf("restore output does not report the pins:\n%s", out)
	}
	for _, canary := range []string{"pins-comment-canary", "pins-token-canary", "BEGIN CERTIFICATE", "BEGIN OPENSSH"} {
		if strings.Contains(plan+out, canary) {
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
	if strings.Contains(out, "known_hosts pins of this server") || strings.Contains(out, "backup's server CA") {
		t.Fatalf("current pins were reported:\n%s", out)
	}
	key, _ := newEd25519HostKey(t)
	pinned := target.bao.kv["release/git-known-hosts"]["known_hosts"].(string)
	unchanged, rewrites, _ := rewriteHostKeyPins(kvExport{Secrets: []kvSecretExport{{Path: "p", Data: map[string]any{"known_hosts": pinned}}}}, []string{"[elsewhere]:22"}, key)
	if len(rewrites) != 0 || unchanged.Secrets[0].Data["known_hosts"] != pinned {
		t.Fatal("a known_hosts without this server's names was rewritten")
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
