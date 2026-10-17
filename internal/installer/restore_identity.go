package installer

// Runtime identities a restore treats with care (#068): the forge (upstream)
// key and the server's SSH host key, both kept in OpenBao KV under
// identities/<namespace>/.

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// identityRestorePlan says what a restore does with the runtime identities.
type identityRestorePlan struct {
	// ForgeKeyKept is true when the target's forge key is kept instead of
	// the archived one; ForgeKeyPath is then left out of the KV restore.
	ForgeKeyKept     bool
	ForgeKeyPath     string
	ForgeKeyHave     string
	ForgeKeyArchived string
	// HostKeyHave and HostKeyArchived are SSH host key fingerprints; they
	// differ when the restore changes the server's SSH host key.
	HostKeyHave     string
	HostKeyArchived string
}

func forgeKeyKVPath(namespace string) string {
	return "identities/" + namespace + "/oberth-upstream-key"
}

func serverIdentityKVPath(namespace string) string { return "identities/" + namespace + "/server" }

// planIdentityRestore compares the archived identities with the target's.
// The forge key the target holds was registered with the forges by its
// install and verified there; the archived one may have been removed from
// them since. Unless restoreForgeKey is set, a differing target key is kept
// and the returned KV export leaves the archived one out.
func planIdentityRestore(ctx context.Context, bao openBaoExec, token, namespace string, kv kvExport, restoreForgeKey bool) (identityRestorePlan, kvExport, error) {
	plan := identityRestorePlan{ForgeKeyPath: forgeKeyKVPath(namespace)}
	forgePath, hostPath := forgeKeyKVPath(namespace), serverIdentityKVPath(namespace)
	filtered := kvExport{Mount: kv.Mount, Secrets: make([]kvSecretExport, 0, len(kv.Secrets))}
	for _, secret := range kv.Secrets {
		switch secret.Path {
		case forgePath:
			have, err := readKVData(ctx, bao, token, kv.Mount, forgePath)
			if err != nil {
				return plan, kv, err
			}
			plan.ForgeKeyArchived = authorizedKeyFingerprint(stringField(secret.Data, "id_ed25519.pub"))
			plan.ForgeKeyHave = authorizedKeyFingerprint(stringField(have, "id_ed25519.pub"))
			if have != nil && plan.ForgeKeyHave != plan.ForgeKeyArchived && !restoreForgeKey {
				plan.ForgeKeyKept = true
				continue
			}
		case hostPath:
			have, err := readKVData(ctx, bao, token, kv.Mount, hostPath)
			if err != nil {
				return plan, kv, err
			}
			plan.HostKeyArchived = sshPrivateFingerprint(stringField(secret.Data, "ssh_host_key"))
			plan.HostKeyHave = sshPrivateFingerprint(stringField(have, "ssh_host_key"))
		}
		filtered.Secrets = append(filtered.Secrets, secret)
	}
	return plan, filtered, nil
}

// identityPlanLines describes the identity decisions for the restore plan.
func identityPlanLines(plan identityRestorePlan, namespace string) []string {
	var lines []string
	switch {
	case plan.ForgeKeyKept:
		lines = append(lines, fmt.Sprintf("Forge key  keeping this install's key %s, which its forges accept; the archive's %s is not restored (--restore-forge-key restores it)",
			displayOr(plan.ForgeKeyHave, "(unreadable)"), displayOr(plan.ForgeKeyArchived, "(unreadable)")))
	case plan.ForgeKeyArchived != "" && plan.ForgeKeyHave != "" && plan.ForgeKeyArchived != plan.ForgeKeyHave:
		lines = append(lines, fmt.Sprintf("Forge key  replacing %s with the archive's %s (--restore-forge-key): every forge must still accept it",
			plan.ForgeKeyHave, plan.ForgeKeyArchived))
	}
	if plan.HostKeyArchived != "" && plan.HostKeyHave != "" && plan.HostKeyArchived != plan.HostKeyHave {
		lines = append(lines, fmt.Sprintf("SSH host   the server's host key returns to the archive's %s (now %s): clients that connected to this install since it was set up must drop the entry, e.g. ssh-keygen -R '[<host>]:30022'",
			plan.HostKeyArchived, plan.HostKeyHave))
	}
	return lines
}

func readKVData(ctx context.Context, bao openBaoExec, token, mount, path string) (map[string]any, error) {
	envelope, err := bao.readData(ctx, token, mount+"/data/"+path)
	if err != nil || envelope == nil {
		return nil, err
	}
	data, _ := envelope["data"].(map[string]any)
	return data, nil
}

func stringField(data map[string]any, key string) string {
	value, _ := data[key].(string)
	return value
}

// authorizedKeyFingerprint is the SHA256 fingerprint of an authorized_keys
// line, or "" when it does not parse.
func authorizedKeyFingerprint(line string) string {
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(line)))
	if err != nil {
		return ""
	}
	return ssh.FingerprintSHA256(key)
}

// sshPrivateFingerprint is the SHA256 fingerprint of a private key's public
// half, or "" when it does not parse.
func sshPrivateFingerprint(pemKey string) string {
	fingerprint, err := sshPublicFingerprint([]byte(pemKey))
	if err != nil {
		return ""
	}
	return fingerprint
}

// readServerCA returns the PEM certificate of the target's server PKI CA,
// which every leaf the server issues chains to, or "" when it cannot be read.
func readServerCA(ctx context.Context, bao openBaoExec, token, mount string) string {
	ca, err := bao.readData(ctx, token, mount+"/cert/ca")
	if err != nil {
		return ""
	}
	certificate, _ := ca["certificate"].(string)
	return strings.TrimSpace(certificate)
}
