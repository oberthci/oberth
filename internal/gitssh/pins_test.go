package gitssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func key(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestPinnedAlgorithmsOnlyNameConfiguredEndpoints(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known_hosts")
	pinned := key(t)
	other := key(t)
	data := append([]byte("[git.example]:2222 "), ssh.MarshalAuthorizedKey(pinned)...)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	verify, algorithms, cleanup, err := pins(path, "git.example:2222", dir, readConfined)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if !reflect.DeepEqual(algorithms, []string{ssh.KeyAlgoED25519}) {
		t.Fatal(algorithms)
	}
	remote := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 2222}
	if err = verify("git.example:2222", remote, pinned); err != nil {
		t.Fatal(err)
	}
	if err = verify("git.example:2222", remote, other); err == nil {
		t.Fatal("unconfigured host key accepted")
	}
	if _, _, clean, err := pins(path, "other.example:2222", dir, readConfined); err == nil {
		clean()
		t.Fatal("unpinned endpoint offered algorithms")
	}
}

func TestConfiguredIdentitiesSelectExactHost(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ssh_config")
	body := "Host first\n IdentityFile /keys/first\n UserKnownHostsFile /pins/first\n IdentitiesOnly yes\n BatchMode yes\n StrictHostKeyChecking yes\n GlobalKnownHostsFile /dev/null\nHost second\n IdentityFile /keys/second\n UserKnownHostsFile /pins/second\n IdentitiesOnly yes\n BatchMode yes\n StrictHostKeyChecking yes\n GlobalKnownHostsFile /dev/null\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"first", "second"} {
		c, err := Configure(path, Config{Host: host})
		if err != nil || c.Identity != "/keys/"+host || c.KnownHosts != "/pins/"+host {
			t.Fatalf("%+v %v", c, err)
		}
	}
	for _, directive := range []string{"Include /outside", "ProxyCommand anything", "IdentityAgent /agent", "StrictHostKeyChecking no"} {
		data := body + "\n" + directive + "\n"
		if strings.HasPrefix(directive, "Strict") {
			data = strings.Replace(body, "StrictHostKeyChecking yes", directive, 1)
		}
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Configure(path, Config{Host: "first"}); err == nil {
			t.Fatalf("accepted %s", directive)
		}
	}
}
func TestConfiguredFileConfinementAndSizeBoundary(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "escape")
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, err := readConfined(path, true); err == nil {
		t.Fatal("escaping symlink accepted")
	}
	for _, size := range []int{1 << 20, (1 << 20) + 1} {
		p := filepath.Join(dir, "bounded")
		if err := os.WriteFile(p, []byte(strings.Repeat("a", size)), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := readConfined(p, true)
		if (err == nil) != (size == 1<<20) {
			t.Fatalf("size %d: %v", size, err)
		}
	}
}
