package client

import (
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
)

type authorityBinding struct {
	base, dir, override, address string
	calls, bearers               *atomic.Int32
	ca                           []byte
}

func newAuthorityBinding(t *testing.T) authorityBinding {
	t.Helper()
	calls, bearers := &atomic.Int32{}, &atomic.Int32{}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "" {
			bearers.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"structuredContent":{"repositories":[]}}}`)
	}))
	server.TLS = &tls.Config{ClientAuth: tls.RequestClientCert}
	server.StartTLS()
	t.Cleanup(server.Close)
	base := t.TempDir()
	if err := os.Chmod(base, 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "selected")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	profile, _ := json.Marshal(Profile{ServerURL: server.URL, Uplink: true})
	selectedCA, _ := certificate(t, 177)
	for name, value := range map[string][]byte{"installation.json": profile, "ca.crt": selectedCA, "token": []byte("private-profile-bearer")} {
		if err := WritePrivate(filepath.Join(dir, name), value); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("selected", filepath.Join(base, "current")); err != nil {
		t.Fatal(err)
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	override := filepath.Join(base, "other-ca.crt")
	if err := WritePrivate(override, ca); err != nil {
		t.Fatal(err)
	}
	return authorityBinding{base, dir, override, server.URL, calls, bearers, ca}
}

func profileAuthorityOverrideCannotBorrowSavedIdentity(t *testing.T) {
	for _, source := range []string{"flag", "environment"} {
		t.Run(source, func(t *testing.T) {
			f := newAuthorityBinding(t)
			options := Options{}
			if source == "flag" {
				options.CAFile = f.override
			}
			env := func(name string) string {
				if source == "environment" && name == "OBERTH_CA_FILE" {
					return f.override
				}
				return ""
			}
			options, err := Resolve(options, f.base, env)
			if err == nil {
				callAuthoritySink(t, options)
			}
			if err == nil || options.TokenFile != "" || f.calls.Load() != 0 || f.bearers.Load() != 0 {
				t.Fatal("different authority borrowed saved identity", err, f.calls.Load(), f.bearers.Load())
			}
		})
	}
}

func equivalentAuthorityAndStandaloneIdentityBinding(t *testing.T) {
	f := newAuthorityBinding(t)
	options, err := Resolve(Options{URL: f.address, CAFile: f.override}, "", func(string) string { return "" })
	if err != nil || options.TokenFile != "" || options.ClientKey != "" || options.ClientCert != "" {
		t.Fatal("standalone transport borrowed identity", err)
	}
	callAuthoritySink(t, options)
	if f.calls.Load() != 1 || f.bearers.Load() != 0 {
		t.Fatal("standalone request used saved credentials")
	}
	if err = WritePrivate(filepath.Join(f.dir, "ca.crt"), append([]byte("\n"), f.ca...)); err != nil {
		t.Fatal(err)
	}
	options, err = Resolve(Options{CAFile: f.override}, f.base, func(string) string { return "" })
	if err != nil || options.TokenFile == "" {
		t.Fatal("same authority from another file was refused", err)
	}
	callAuthoritySink(t, options)
	if f.calls.Load() != 2 || f.bearers.Load() != 1 {
		t.Fatal("equivalent authority lost its saved identity")
	}
}

func callAuthoritySink(t *testing.T, options Options) {
	t.Helper()
	c, err := New(Config{Options: options, Now: fake.NewClock().Now})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, _, err = c.Call(t.Context(), definition(t, "repo_list"), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
}

func resolvedAuthoritySurvivesOverrideReplacement(t *testing.T) {
	for _, identity := range []string{"bearer", "certificate"} {
		for _, replacement := range []string{"file", "parent-link"} {
			t.Run(identity+"/"+replacement, func(t *testing.T) { resolvedAuthoritySwapCase(t, identity, replacement) })
		}
	}
}

func resolvedAuthoritySwapCase(t *testing.T, identity, replacement string) {
	t.Helper()
	f := newAuthorityBinding(t)
	if identity == "certificate" {
		f.useCertificate(t)
	}
	override := f.prepareOverride(t, replacement)
	options, err := Resolve(Options{CAFile: override}, f.base, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if identity == "certificate" && (options.ClientCert == "" || options.ClientKey == "") {
		t.Fatal("certificate identity missing")
	}
	f.replaceOverride(t, replacement, override)
	c, err := New(Config{Options: options, Now: fake.NewClock().Now})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _, err = c.Call(t.Context(), definition(t, "repo_list"), json.RawMessage(`{}`))
	if err == nil || f.calls.Load() != 0 || f.bearers.Load() != 0 {
		t.Fatal("replacement authority received saved identity", err, f.calls.Load(), f.bearers.Load())
	}
}

func (f authorityBinding) useCertificate(t *testing.T) {
	t.Helper()
	if err := os.Remove(filepath.Join(f.dir, "token")); err != nil {
		t.Fatal(err)
	}
	cert, key := certificate(t, 178)
	for name, data := range map[string][]byte{"admin.crt": cert, "admin.key": key} {
		if err := WritePrivate(filepath.Join(f.dir, name), data); err != nil {
			t.Fatal(err)
		}
	}
}

func (f authorityBinding) prepareOverride(t *testing.T, replacement string) string {
	t.Helper()
	selected, err := ReadPublic(filepath.Join(f.dir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if replacement == "file" {
		if err := WritePrivate(f.override, selected); err != nil {
			t.Fatal(err)
		}
		return f.override
	}
	for name, data := range map[string][]byte{"original": selected, "replacement": f.ca} {
		dir := filepath.Join(f.base, name, "nested")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := WritePrivate(filepath.Join(dir, "ca.crt"), data); err != nil {
			t.Fatal(err)
		}
	}
	// A symlink ancestor is allowed; the immediate directory and file remain regular.
	if err := os.Symlink("original", filepath.Join(f.base, "link")); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(f.base, "link", "nested", "ca.crt")
}

func (f authorityBinding) replaceOverride(t *testing.T, replacement, override string) {
	t.Helper()
	if replacement == "file" {
		if err := WritePrivate(override, f.ca); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.Symlink("replacement", filepath.Join(f.base, "next-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(f.base, "next-link"), filepath.Join(f.base, "link")); err != nil {
		t.Fatal(err)
	}
}

func authorityOverrideWithAdditionalRootIsRefused(t *testing.T) {
	f := newAuthorityBinding(t)
	selected, err := ReadPublic(filepath.Join(f.dir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := WritePrivate(f.override, append(selected, f.ca...)); err != nil {
		t.Fatal(err)
	}
	options, err := Resolve(Options{CAFile: f.override}, f.base, func(string) string { return "" })
	if err == nil || options.TokenFile != "" || f.calls.Load() != 0 || f.bearers.Load() != 0 {
		t.Fatal("superset authority borrowed saved identity", err, f.calls.Load(), f.bearers.Load())
	}
}

// Covers: REQ-C11-013
func TestInstallationBoundClientAuthorityContract(t *testing.T) {
	t.Run("transport", installationBoundTransportContract)
	t.Run("override-mismatch", profileAuthorityOverrideCannotBorrowSavedIdentity)
	t.Run("equivalent-and-standalone", equivalentAuthorityAndStandaloneIdentityBinding)
	t.Run("override-replacement", resolvedAuthoritySurvivesOverrideReplacement)
	t.Run("additional-root", authorityOverrideWithAdditionalRootIsRefused)
}
