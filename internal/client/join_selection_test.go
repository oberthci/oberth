package client

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestJoinKeepsSameInstallationAdministrator(t *testing.T) {
	for _, kind := range []string{"administrator", "uplink", "other-address", "other-ca", "no-current"} {
		t.Run(kind, func(t *testing.T) { joinSelectionCase(t, kind) })
	}
}
func joinSelectionCase(t *testing.T, kind string) {
	t.Helper()
	base := t.TempDir()
	joined := filepath.Join(base, "agent")
	f := newEnrollment(t, false, joined)
	metadata := selectedJoinFixture(t, f, kind, base)
	if _, err := Join(t.Context(), f.config, f.invitation, joined); err != nil {
		t.Fatal(err)
	}
	selected, err := os.Readlink(filepath.Join(base, "current"))
	want := "agent"
	if kind == "administrator" {
		want = "install"
	}
	if err != nil || selected != want {
		t.Fatalf("selected %q, want %q: %v", selected, want, err)
	}
	if *f.calls != 1 {
		t.Fatal("enrollment repeated")
	}
	if kind == "administrator" {
		assertAdministratorOptions(t, base, metadata)
	}
	profile, err := readProfile(joined)
	if err != nil || !profile.Uplink || profile.AdminKey != "" || profile.AdminCert != "" {
		t.Fatal("joined profile is not explicitly an uplink")
	}
	assertNoSelectionTemporary(t, base)
}

func selectedJoinFixture(t *testing.T, f enrollmentFixture, kind, base string) Profile {
	t.Helper()
	install := filepath.Join(base, "install")
	if err := os.Mkdir(install, 0700); err != nil {
		t.Fatal(err)
	}
	ca := f.authority
	if kind == "other-ca" {
		ca, _ = certificate(t, 99)
	}
	metadata := Profile{ServerURL: f.invitation.ServerURL, SSHURL: "ssh://git@oberth.local:2222", AdminKey: filepath.Join(install, "admin-version.key"), AdminCert: filepath.Join(install, "admin-version.crt"), CAFile: filepath.Join(install, "ca.crt")}
	if kind == "uplink" {
		metadata.AdminKey, metadata.AdminCert = "", ""
	}
	if kind == "other-address" {
		metadata.ServerURL = "https://another.local"
	}
	raw, _ := json.Marshal(metadata)
	for name, data := range map[string][]byte{"installation.json": raw, "ca.crt": ca, "admin-version.key": []byte("retained-key"), "admin-version.crt": []byte("retained-cert")} {
		if err := WritePrivate(filepath.Join(install, name), data); err != nil {
			t.Fatal(err)
		}
	}
	if kind != "no-current" {
		if err := os.Symlink("install", filepath.Join(base, "current")); err != nil {
			t.Fatal(err)
		}
	}
	return metadata
}
func assertAdministratorOptions(t *testing.T, base string, metadata Profile) {
	t.Helper()
	options, err := Resolve(Options{}, base, func(string) string { return "" })
	if err != nil || options.ClientKey != metadata.AdminKey || options.ClientCert != metadata.AdminCert {
		t.Fatalf("administrator unavailable without override: %+v %v", options, err)
	}
}
func assertNoSelectionTemporary(t *testing.T, base string) {
	t.Helper()
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if len(entry.Name()) >= 9 && entry.Name()[:9] == ".current-" {
			t.Fatal("temporary selection remains")
		}
	}
}
