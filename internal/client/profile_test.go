package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Covers: REQ-C11-007
func TestOwnedPrivateCliFilesContract(t *testing.T) {
	path := privateFile(t, "credential", []byte("first"))
	if err := WritePrivate(path, []byte("second")); err != nil {
		t.Fatal(err)
	}
	data, err := ReadPrivate(path)
	info, statErr := os.Stat(path)
	if err != nil || statErr != nil || string(data) != "second" || info.Mode().Perm() != 0600 {
		t.Fatal("private write failed")
	}
	assertPrivateModes(t, path)
	assertPrivateLinks(t, path)
}

func assertPrivateLinks(t *testing.T, path string) {
	t.Helper()
	var err error
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(path), "link")
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if WritePrivate(link, []byte("bad")) == nil {
		t.Fatal("write followed link")
	}
	if _, err = ReadPrivate(link); err == nil {
		t.Fatal("read followed link")
	}
	if err = os.Chmod(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if WritePrivate(path, nil) == nil {
		t.Fatal("write accepted public directory")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".oberth-") {
			t.Fatal("temporary file remains")
		}
	}
}

func TestProfileSelectionAndPrecedence(t *testing.T) {
	base, dir := profileFixture(t)
	var err error
	env := func(string) string { return "" }
	got, err := Resolve(Options{}, base, env)
	if err != nil || got.TokenFile != filepath.Join(dir, "token") || got.ClientKey != filepath.Join(dir, "admin.key") {
		t.Fatalf("profile not selected: %+v %v", got, err)
	}
	assertProfileOverride(t, base)
	got, err = Resolve(Options{URL: "https://oberth.local:443/", TokenFile: "explicit"}, base, env)
	if err != nil || got.TokenFile != "explicit" || got.CAFile == "" {
		t.Fatal("equivalent address or explicit precedence lost")
	}
	assertUntrustedProfile(t, base, dir)
}

func assertPrivateModes(t *testing.T, path string) {
	t.Helper()
	var err error
	for _, mode := range []os.FileMode{0640, 0604, 0660, 0606} {
		if err = os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if WritePrivate(path, []byte("replacement")) == nil {
			t.Fatalf("accepted mode %o", mode)
		}
		if _, err = ReadPrivate(path); err == nil {
			t.Fatalf("read mode %o", mode)
		}
		original, readErr := os.ReadFile(path)
		if readErr != nil || string(original) != "second" {
			t.Fatal("refused file changed")
		}
	}
}

func profileFixture(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	if err := os.Chmod(base, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := ProfileDir(base, "context")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"installation.json": `{"server_url":"https://oberth.local"}`, "token": "valid", "ca.crt": "ca", "admin.crt": "cert", "admin.key": "key"} {
		if err = WritePrivate(filepath.Join(dir, name), []byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.Symlink("context", filepath.Join(base, "current")); err != nil {
		t.Fatal(err)
	}
	return base, dir
}

func assertUntrustedProfile(t *testing.T, base, dir string) {
	t.Helper()
	var err error
	for _, name := range []string{"../escape", ".", "/absolute", "nested/name"} {
		if _, err = ProfileDir(base, name); err == nil {
			t.Fatal("invalid profile name accepted")
		}
	}
	if err = os.Chmod(filepath.Join(dir, "installation.json"), 0666); err != nil {
		t.Fatal(err)
	}
	if _, err = Resolve(Options{}, base, func(string) string { return "" }); err == nil {
		t.Fatal("untrusted metadata accepted")
	}
}

func assertProfileOverride(t *testing.T, base string) {
	t.Helper()
	env := func(string) string { return "" }
	var got Options
	var err error
	for _, address := range []string{"https://other.local", "https://oberth.local/other", "https://oberth.local:444"} {
		got, err = Resolve(Options{URL: address}, base, env)
		if err != nil || got.TokenFile != "" || got.CAFile != "" || got.ClientCert != "" || got.ClientKey != "" {
			t.Fatal("saved authority followed override")
		}
	}
}

func TestPrivateReadBoundAndEnvironment(t *testing.T) {
	for _, size := range []int{1 << 20, 1<<20 + 1} {
		path := privateFile(t, "bounded", []byte(strings.Repeat("x", size)))
		_, err := ReadPrivate(path)
		if (err != nil) != (size > 1<<20) {
			t.Fatalf("size %d: %v", size, err)
		}
	}
	got, err := Resolve(Options{URL: "https://explicit.local"}, "", func(name string) string {
		if name == "OBERTH_URL" {
			return "https://environment.local"
		}
		if name == "OBERTH_TOKEN_FILE" {
			return "environment-token"
		}
		return ""
	})
	if err != nil || got.URL != "https://explicit.local" || got.TokenFile != "environment-token" {
		t.Fatal("environment precedence lost")
	}
}
