package gitssh

import (
	"crypto/rand"
	"crypto/rsa"
	"golang.org/x/crypto/ssh"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestReviewSecondCommandAndPortBoundary(t *testing.T) {
	for _, n := range []int{16384, 16385} {
		_, e := Parse([]string{"git@host", strings.Repeat("a", n)})
		if (e == nil) != (n == 16384) {
			t.Errorf("command %d: %v", n, e)
		}
	}
}

func TestConfiguredPortAcceptsEndpointsRefusesOutsideRange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh-config")
	data := "Host host\n IdentitiesOnly yes\n BatchMode yes\n StrictHostKeyChecking yes\n GlobalKnownHostsFile /dev/null\n IdentityFile /selected/key\n UserKnownHostsFile /selected/pins\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	for _, port := range []string{"0", "1", "65535", "65536"} {
		_, err := Configure(path, Config{Host: "host", Port: port})
		if (err == nil) != (port == "1" || port == "65535") {
			t.Fatalf("port %s: %v", port, err)
		}
	}
}

func TestPrivateIdentityPermissionsAndDashHostAreRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("key"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readConfined(path, true); err == nil {
		t.Fatal("publicly readable private key accepted")
	}
	if _, err := Parse([]string{"-oProxyCommand=x", "command"}); err == nil {
		t.Fatal("dash host accepted")
	}
}
func TestRSAPinsNeverOfferSHA1(t *testing.T) {
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	public, err := ssh.NewPublicKey(&private.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "pins")
	if err = os.WriteFile(path, append([]byte("host "), ssh.MarshalAuthorizedKey(public)...), 0600); err != nil {
		t.Fatal(err)
	}
	_, algorithms, cleanup, err := pins(path, "host:22", dir, readConfined)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if slices.Contains(algorithms, ssh.KeyAlgoRSA) || !slices.Contains(algorithms, ssh.KeyAlgoRSASHA256) {
		t.Fatal("unsafe RSA algorithm set", algorithms)
	}
}
