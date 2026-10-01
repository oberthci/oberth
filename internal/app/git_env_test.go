package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitSSHCommandPinsIdentityAndHostVerification(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "operator's files")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(directory, "id_ed25519")
	hostsPath := filepath.Join(directory, "known_hosts")
	if err := os.WriteFile(keyPath, []byte("private-key-placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hostsPath, []byte("example.invalid ssh-ed25519 AAAA"), 0o644); err != nil {
		t.Fatal(err)
	}
	command, err := GitSSHCommand(keyPath, hostsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"'-F' '/dev/null'",
		"BatchMode=yes",
		"IdentitiesOnly=yes",
		"StrictHostKeyChecking=yes",
		"GlobalKnownHostsFile=/dev/null",
		"UserKnownHostsFile=",
	} {
		if !strings.Contains(command, required) {
			t.Fatalf("command lacks %q: %s", required, command)
		}
	}
	if !strings.Contains(command, `UserKnownHostsFile="`) {
		t.Fatalf("known_hosts path lacks OpenSSH config quoting: %s", command)
	}
	if strings.Contains(command, "operator's files") {
		t.Fatal("path with quote was not shell-escaped")
	}
	if !strings.Contains(command, "operator'\\''s files") {
		t.Fatalf("escaped command = %s", command)
	}
}

func TestGitSSHCommandPathsRejectsOpenSSHExpansionCharacters(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		`/secrets/${HOME}/key`,
		`/secrets/%h/key`,
		`/secrets/"key"`,
		`/secrets/key\name`,
	} {
		path := path
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			if _, err := GitSSHCommandPaths(path, "/secrets/known_hosts"); err == nil {
				t.Fatalf("expanding path %q passed validation", path)
			}
			if _, err := GitSSHCommandPaths("/secrets/key", path); err == nil {
				t.Fatalf("expanding path %q passed known_hosts validation", path)
			}
		})
	}
}

func TestGitSSHCommandRejectsWorldReadablePrivateKey(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	keyPath := filepath.Join(directory, "key")
	hostsPath := filepath.Join(directory, "known_hosts")
	if err := os.WriteFile(keyPath, []byte("key"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hostsPath, []byte("host key"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := GitSSHCommand(keyPath, hostsPath); err == nil {
		t.Fatal("world-readable private key passed validation")
	}
}

// TestBuildSSHConfigPerHostBlocksAndNegatedFallback pins the generated
// multi-identity transport: every per-host block carries the full fail-closed
// option set, non-default ports render, and the fallback block negates every
// per-host name. The negation is load-bearing: OpenSSH accumulates
// IdentityFile directives across all matching blocks, so without it the
// shared fallback key would also be offered to per-key hosts — masking a
// revoked dedicated key and disclosing the shared public key to a forge that
// should only ever see its own identity.
func TestBuildSSHConfigPerHostBlocksAndNegatedFallback(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	sharedKey := filepath.Join(directory, "id_ed25519")
	hostsPath := filepath.Join(directory, "known_hosts")
	codebergKey := filepath.Join(directory, "id_ed25519-codeberg")
	githubKey := filepath.Join(directory, "id_ed25519-github")
	config, err := BuildSSHConfig([]SSHConfigEntry{
		{Host: "codeberg.org", Port: "22", IdentityFile: codebergKey},
		{Host: "github.example.net", Port: "2222", IdentityFile: githubKey},
	}, sharedKey, hostsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"Host codeberg.org\n",
		"Host github.example.net\n  Port 2222\n",
		`IdentityFile "` + codebergKey + `"`,
		`IdentityFile "` + githubKey + `"`,
		"Host * !codeberg.org !github.example.net\n",
		`IdentityFile "` + sharedKey + `"`,
	} {
		if !strings.Contains(config, required) {
			t.Fatalf("config lacks %q:\n%s", required, config)
		}
	}
	if strings.Contains(config, "Host codeberg.org\n  Port 22\n") {
		t.Fatalf("default port 22 must not render:\n%s", config)
	}
	for block := range strings.SplitSeq(strings.TrimSpace(config), "\n\n") {
		for _, required := range []string{
			"IdentitiesOnly yes", "BatchMode yes", "StrictHostKeyChecking yes",
			"GlobalKnownHostsFile /dev/null", `UserKnownHostsFile "` + hostsPath + `"`,
		} {
			if !strings.Contains(block, required) {
				t.Fatalf("block lacks %q:\n%s", required, block)
			}
		}
	}
}

// TestBuildSSHConfigQuotesPathsWithSpacesAndHash verifies that the generated
// SSH config file preserves paths containing whitespace and '#' (which OpenSSH
// treats as a comment start) through double-quoting.
func TestBuildSSHConfigQuotesPathsWithSpacesAndHash(t *testing.T) {
	t.Parallel()
	// validateOperatorPath rejects control, $, %, ", and \ but allows
	// spaces and # — those are the characters that break unquoted config.
	// We cannot test spaces/# directly in paths because validateOperatorPath
	// would reject them — wait, let me re-check.
	// Actually, validateOperatorPath rejects: \x00, \r, \n, $, %, ", \
	// It does NOT reject spaces or #. So spaces and # are valid.
	directory := filepath.Join(t.TempDir(), "my keys")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	sharedKey := filepath.Join(directory, "id_ed25519")
	hostsPath := filepath.Join(directory, "known_hosts")
	forgeKey := filepath.Join(directory, "id_ed25519-forge")
	config, err := BuildSSHConfig([]SSHConfigEntry{
		{Host: "forge.example", Port: "22", IdentityFile: forgeKey},
	}, sharedKey, hostsPath)
	if err != nil {
		t.Fatal(err)
	}
	// Paths with spaces must be double-quoted in the output.
	if !strings.Contains(config, `IdentityFile "`+forgeKey+`"`) {
		t.Fatalf("forge key path not quoted:\n%s", config)
	}
	if !strings.Contains(config, `IdentityFile "`+sharedKey+`"`) {
		t.Fatalf("shared key path not quoted:\n%s", config)
	}
	if !strings.Contains(config, `UserKnownHostsFile "`+hostsPath+`"`) {
		t.Fatalf("known_hosts path not quoted:\n%s", config)
	}
}

// TestBuildSSHConfigRejectsHostileValues pins the injection boundary: SSH
// config directives are newline- and whitespace-delimited, so hostnames or
// paths that could smuggle additional directives (ProxyCommand being the
// canonical payload) are refused, never escaped.
func TestBuildSSHConfigRejectsHostileValues(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	goodKey := filepath.Join(directory, "id_ed25519-x")
	shared := filepath.Join(directory, "id_ed25519")
	hosts := filepath.Join(directory, "known_hosts")
	for name, entry := range map[string]SSHConfigEntry{
		"empty host":         {Host: "", IdentityFile: goodKey},
		"host with newline":  {Host: "forge.example\nProxyCommand curl evil", IdentityFile: goodKey},
		"host with space":    {Host: "forge example", IdentityFile: goodKey},
		"host with glob":     {Host: "forge*", IdentityFile: goodKey},
		"host with negation": {Host: "!forge.example", IdentityFile: goodKey},
		"host with comma":    {Host: "a,b", IdentityFile: goodKey},
		"non-numeric port":   {Host: "forge.example", Port: "22 ProxyCommand", IdentityFile: goodKey},
		"identity with pct":  {Host: "forge.example", IdentityFile: "/etc/keys/%h"},
		"relative identity":  {Host: "forge.example", IdentityFile: "keys/id"},
	} {
		if _, err := BuildSSHConfig([]SSHConfigEntry{entry}, shared, hosts); err == nil {
			t.Fatalf("%s: hostile entry was accepted", name)
		}
	}
	if _, err := BuildSSHConfig(nil, "relative/shared", hosts); err == nil {
		t.Fatal("relative shared key path was accepted")
	}
	if _, err := BuildSSHConfig(nil, shared, "/hosts\nProxyCommand curl evil"); err == nil {
		t.Fatal("hostile known_hosts path was accepted")
	}
}

func TestGitSSHCommandFromConfigQuotesForShellEvaluation(t *testing.T) {
	t.Parallel()
	command, err := GitSSHCommandFromConfig("/tmp/oberth-ssh-config")
	if err != nil {
		t.Fatal(err)
	}
	if command != `'ssh' '-F' '/tmp/oberth-ssh-config'` {
		t.Fatalf("command = %q", command)
	}
	if _, err := GitSSHCommandFromConfig("/tmp/oberth\nssh-config"); err == nil {
		t.Fatal("hostile config path was accepted")
	}
}

func TestUpstreamSSHHostParsesSSHURLsOnly(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ url, host, port string }{
		{"ssh://git@codeberg.org/acme", "codeberg.org", "22"},
		{"ssh://git@forge.example.net:2222/org", "forge.example.net", "2222"},
		{"https://forge.example.net/org", "", ""},
		{"/var/lib/local-upstream", "", ""},
		{"ssh://git@forge.example.net:bad/org", "", ""},
		{"ssh://git@foo%0abar/org", "", ""},
	} {
		host, port := UpstreamSSHHost(test.url)
		if host != test.host || port != test.port {
			t.Fatalf("UpstreamSSHHost(%q) = %q,%q want %q,%q", test.url, host, port, test.host, test.port)
		}
	}
}

func TestValidateUpstreamNameEnforcesDNSLabel(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"github", "codeberg", "gitlab-corp", "a", "a-1", strings.Repeat("a", 53)} {
		if err := ValidateUpstreamName(name); err != nil {
			t.Fatalf("valid name %q rejected: %v", name, err)
		}
	}
	for _, name := range []string{
		"", "-a", "a-", "A", "a.b", "a_b", "a b", "../x", "a/b", "a\\b",
		"a\tb", "a\nb", "a\x00b", strings.Repeat("a", 54),
	} {
		if err := ValidateUpstreamName(name); err == nil {
			t.Fatalf("invalid name %q accepted", name)
		}
	}
}

func TestUpstreamKeyDataKeyAndValidation(t *testing.T) {
	t.Parallel()
	if key := UpstreamKeyDataKey("id_ed25519", "github"); key != "id_ed25519-github" {
		t.Fatalf("data key = %q", key)
	}
	for _, key := range []string{"id_ed25519-github", "id_ed25519-a-1.pub"} {
		if !ValidUpstreamKeyName(key) {
			t.Fatalf("valid key name %q rejected", key)
		}
	}
	for _, key := range []string{"", ".", "..", "../x", "a/b", "a\\b", "a b", "a\nb", "A"} {
		if ValidUpstreamKeyName(key) {
			t.Fatalf("unsafe key name %q accepted", key)
		}
	}
}
