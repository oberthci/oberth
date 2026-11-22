package installer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/oberthci/oberth/internal/clientcert"
)

func (c *ConfigDir) profile() (Profile, error) {
	b, err := c.read("installation.json")
	if err != nil {
		return Profile{}, err
	}
	var p Profile
	if err = json.Unmarshal(b, &p); err != nil {
		return p, err
	}
	if p.answersOnly() {
		return p, nil
	}
	for _, file := range []string{p.AdminKey, p.AdminCert, p.KnownHosts} {
		if filepath.Dir(file) != c.path {
			return p, problem("custody", nil)
		}
	}
	return p, nil
}
func (e *Engine) saveProfile(p *Plan, i Identities, a admin) (Profile, error) {
	c := e.Credentials
	profile := Profile{
		Version: 1, ServerVersion: p.release.Version(), Context: p.choices.Context,
		ClusterUID: p.uid, ServerURL: p.choices.URL, MCPURL: p.choices.URL + "/mcp",
		BootstrapPending:  true,
		Ready:             p.previous.Ready,
		Namespace:         p.choices.Namespace,
		PipelineNamespace: p.choices.PipelineNamespace, HTTPSNodePort: p.choices.HTTPSNodePort, SSHNodePort: p.choices.SSHNodePort,
	}
	v, _ := inputs(p.release.chart, p.request.Values, p.choices.Namespace)
	if settingText(v, "secretStore", "bundled") == "on" {
		profile.RecoveryKey = c.pathFor("recovery")
	}
	cert, err := clientcert.ParseCertificate(i.CA.Data["tls.crt"])
	if err != nil {
		return Profile{}, err
	}
	profile.CAPin = clientcert.Pin(cert)
	u, _ := url.Parse(p.choices.URL)
	address := net.JoinHostPort(u.Hostname(), strconv.Itoa(p.choices.SSHPort))
	profile.SSHURL = "ssh://git@" + address
	host := []byte("[" + u.Hostname() + "]:" + strconv.Itoa(p.choices.SSHPort) + " " + string(i.Host.Data["public"]))
	hostName := "known_hosts-" + publicDigest(host)
	caName := "ca-" + publicDigest(i.CA.Data["tls.crt"]) + ".crt"
	// The key file is named by its public key, so a reused key keeps its file;
	// each apply issues a new certificate under a new name.
	keyName, certName := "admin-"+publicDigest(a.public)+".key", "admin-"+a.id+".crt"
	// Every apply writes the stable key name, so it is replaced whole.
	if err = c.replace(keyName, "admin-"+a.id+".key.tmp", a.key); err != nil {
		return Profile{}, err
	}
	for name, data := range map[string][]byte{
		certName: a.cert, caName: i.CA.Data["tls.crt"], hostName: host,
	} {
		if err = c.save(name, data); err != nil {
			return Profile{}, err
		}
	}
	profile.BackupRecipient, err = e.backupKey()
	if err != nil {
		return Profile{}, err
	}
	profile.AdminKey, profile.AdminCert = c.pathFor(keyName), c.pathFor(certName)
	profile.CAFile, profile.KnownHosts = c.pathFor(caName), c.pathFor(hostName)
	profile.BackupKey = c.pathFor("backup.agekey")
	return profile, c.saveProfile(profile, a.id)
}
func publicDigest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

// prune removes every earlier credential file of this profile except those the
// Git helper still names, including copies an earlier failed repair kept.
func (c *ConfigDir) prune(current Profile, helper []string) error {
	earlier, err := EarlierCredentials(current)
	if err != nil {
		return problem("custody", err)
	}
	for _, path := range earlier {
		if slices.Contains(helper, path) {
			continue
		}
		if err := c.root.Remove(filepath.Base(path)); err != nil && !isAbsent(err) {
			return err
		}
	}
	return c.directory.Sync()
}

// credentialName matches the digest-named files saveProfile writes.
func credentialName(name string) bool {
	for _, shape := range [][2]string{{"admin-", ".key"}, {"admin-", ".crt"}, {"ca-", ".crt"}, {"known_hosts-", ""}} {
		digest, ok := strings.CutPrefix(name, shape[0])
		digest, cut := strings.CutSuffix(digest, shape[1])
		if ok && cut && len(digest) == 64 && strings.Trim(digest, "0123456789abcdef") == "" {
			return true
		}
	}
	return false
}

// EarlierCredentials lists the administrator, CA and host-key files in the
// profile's directory that the profile no longer names.
func EarlierCredentials(p Profile) ([]string, error) {
	dir := filepath.Dir(p.AdminKey)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	current := []string{p.AdminKey, p.AdminCert, p.CAFile, p.KnownHosts}
	var earlier []string
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if entry.Type().IsRegular() && credentialName(entry.Name()) && !slices.Contains(current, path) {
			earlier = append(earlier, path)
		}
	}
	return earlier, nil
}
