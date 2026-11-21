package releaseverify

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func resign(t *testing.T, f fstest.MapFS, key *ecdsa.PrivateKey) {
	t.Helper()
	sums := []byte(fmt.Sprintf("%x  oberth\n%x  release.json\n", sha256.Sum256(f["oberth"].Data), sha256.Sum256(f["release.json"].Data)))
	f["SHA256SUMS"].Data = sums
	f["SHA256SUMS.sigstore.json"].Data = signed(t, key, sums)
}
func TestRecordStrictSchema(t *testing.T) {
	key, pub := signingKey(t)
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"unknown", func(r map[string]any) { r["unknown"] = true }},
		{"case variant", func(r map[string]any) { r["Version"] = r["version"]; delete(r, "version") }},
		{"commit length", func(r map[string]any) { r["commit"] = "bad" }},
		{"commit hex", func(r map[string]any) { r["commit"] = strings.Repeat("A", 40) }},
		{"missing images", func(r map[string]any) { delete(r, "images") }},
		{"null images", func(r map[string]any) { r["images"] = nil }},
		{"extra image", func(r map[string]any) { r["images"].(map[string]any)["extra"] = "sha256:" + strings.Repeat("a", 64) }},
		{"missing image", func(r map[string]any) { delete(r["images"].(map[string]any), "oberth") }},
		{"tag image", func(r map[string]any) { r["images"].(map[string]any)["oberth"] = "latest" }},
		{"upper image", func(r map[string]any) { r["images"].(map[string]any)["oberth"] = "sha256:" + strings.Repeat("A", 64) }},
		{"base", func(r map[string]any) { r["base"] = "latest" }},
		{"unknown component field", func(r map[string]any) { r["components"] = map[string]any{"oberth": map[string]any{"unexpected": true}} }},
		{"unknown git field", func(r map[string]any) { r["git"] = map[string]any{"amd64": map[string]any{"unexpected": true}} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixture(t, key, "v1.0.0")
			var record map[string]any
			if err := json.Unmarshal(f["release.json"].Data, &record); err != nil {
				t.Fatal(err)
			}
			tc.change(record)
			f["release.json"].Data, _ = json.Marshal(record)
			resign(t, f, key)
			if _, err := verify(t.Context(), f, pub, "v1.0.0"); !errors.Is(err, Metadata) {
				t.Fatalf("invalid record: %v", err)
			}
		})
	}
	for _, suffix := range []string{`,"version":"v1.0.0"}`, `,"Version":"v1.0.0"}`, `} {}`} {
		f := fixture(t, key, "v1.0.0")
		f["release.json"].Data = append(bytes.TrimSuffix(f["release.json"].Data, []byte("}")), []byte(suffix)...)
		resign(t, f, key)
		if _, err := verify(t.Context(), f, pub, "v1.0.0"); !errors.Is(err, Metadata) {
			t.Fatalf("duplicate/trailing: %v", err)
		}
	}
}
func TestKeyRequiresP256(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseKey(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})); !errors.Is(err, Key) {
		t.Fatal(err)
	}
}

func TestSelectedArtifactAndExactPipelineSet(t *testing.T) {
	key, pub := signingKey(t)
	for _, kind := range []string{"subset", "extra", "tampered", "missing", "symlink", "directory", "unlisted"} {
		t.Run(kind, func(t *testing.T) {
			f := fixture(t, key, "v1.0.0")
			changeSelected(f, kind)
			release, err := verify(t.Context(), f, pub, "v1.0.0")
			if err != nil {
				t.Fatalf("metadata should not read unused artifacts: %v", err)
			}
			if err := release.CheckFiles(); kind != "unlisted" && err == nil {
				t.Fatal("pipeline accepted changed set")
			}
			name := "oberth"
			if kind == "unlisted" {
				name = "other"
			}
			checkSelectedRead(t, release, name, kind == "extra")
		})
	}
}

func checkSelectedRead(t *testing.T, release *Release, name string, valid bool) {
	t.Helper()
	body, err := release.ReadFile(name, MaxArtifact)
	if valid {
		if err != nil || string(body) != "shipped bytes" {
			t.Fatal(err)
		}
		return
	}
	if err == nil {
		t.Fatal("unchecked artifact accepted")
	}
}

type changingFS struct {
	fstest.MapFS
	opens int
}

func (f *changingFS) Open(name string) (fs.File, error) {
	if name == "release.json" {
		f.opens++
		if f.opens > 1 {
			return nil, fmt.Errorf("record opened twice")
		}
	}
	return f.MapFS.Open(name)
}

// Lstat must not open or consume the record before the actual authenticated read.
func (f *changingFS) Lstat(name string) (fs.FileInfo, error) { return fs.Stat(f.MapFS, name) }
func (f *changingFS) ReadLink(name string) (string, error)   { return "", fs.ErrInvalid }
func TestRecordReturnedFromOneAuthenticatedRead(t *testing.T) {
	key, pub := signingKey(t)
	f := &changingFS{MapFS: fixture(t, key, "v1.0.0")}
	release, err := verify(t.Context(), f, pub, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if release.Record().Commit != strings.Repeat("a", 40) || f.opens != 1 {
		t.Fatal("returned unverified record")
	}
	if _, err := release.ReadFile("release.json", MaxMetadata); err != nil || f.opens != 1 {
		t.Fatal("record reopened", err)
	}
}

type cancelFile struct {
	fs.File
	cancel context.CancelFunc
}

func (f cancelFile) Read(p []byte) (int, error) { n, err := f.File.Read(p); f.cancel(); return n, err }

type cancelFS struct {
	fstest.MapFS
	cancel context.CancelFunc
}

func (f cancelFS) Open(name string) (fs.File, error) {
	file, err := f.MapFS.Open(name)
	if err == nil && name == "oberth" {
		return cancelFile{file, f.cancel}, nil
	}
	return file, err
}
func (f cancelFS) Lstat(name string) (fs.FileInfo, error) { return fs.Stat(f.MapFS, name) }
func (f cancelFS) ReadLink(name string) (string, error)   { return "", fs.ErrInvalid }
func TestHashHonoursCancellationAndClose(t *testing.T) {
	key, pub := signingKey(t)
	f := fixture(t, key, "v1.0.0")
	f["oberth"].Data = bytes.Repeat([]byte("x"), 2<<20)
	resign(t, f, key)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	release, err := verify(ctx, cancelFS{f, cancel}, pub, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := release.Open("oberth")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, reader); !errors.Is(err, context.Canceled) {
		t.Fatal("hash ignored cancellation", err)
	}
	if err := reader.Close(); !errors.Is(err, context.Canceled) {
		t.Fatal("close lost cancellation", err)
	}
}
func TestCloseChecksUnreadTail(t *testing.T) {
	key, pub := signingKey(t)
	f := fixture(t, key, "v1.0.0")
	release, err := verify(t.Context(), f, pub, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	f["oberth"].Data[len(f["oberth"].Data)-1] ^= 1
	reader, err := release.Open("oberth")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); !errors.Is(err, Digest) {
		t.Fatal("unread tail was not verified", err)
	}
}
func TestChecksumNamesAreLowercaseFlatNames(t *testing.T) {
	for _, name := range []string{"-rf", " oberth", "ob\x00erth", "OBERTH", "a/b", "a\\b"} {
		_, err := parseSums([]byte(strings.Repeat("a", 64) + "  " + name + "\n" + strings.Repeat("a", 64) + "  release.json\n"))
		if !errors.Is(err, Inventory) {
			t.Fatalf("name %q: %v", name, err)
		}
	}
}

func changeSelected(f fstest.MapFS, kind string) {
	switch kind {
	case "subset", "missing":
		delete(f, "oberth")
	case "extra":
		f["extra"] = &fstest.MapFile{Data: []byte("ignored")}
	case "tampered":
		f["oberth"].Data[0] ^= 1
	case "symlink":
		f["oberth"].Mode = os.ModeSymlink
	case "directory":
		f["oberth"].Mode = os.ModeDir
	}
}

func TestTamperedLastReadRefusesExactLength(t *testing.T) {
	key, pub := signingKey(t)
	for _, method := range []string{"full", "copy", "read"} {
		t.Run(method, func(t *testing.T) {
			files := fixture(t, key, "v1.0.0")
			release, err := verify(t.Context(), files, pub, "v1.0.0")
			if err != nil {
				t.Fatal(err)
			}
			body := files["oberth"].Data
			body[len(body)-1] ^= 1
			reader, err := release.Open("oberth")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reader.Close() }()
			buffer := make([]byte, len(body))
			switch method {
			case "full":
				_, err = io.ReadFull(reader, buffer)
			case "copy":
				_, err = io.CopyN(io.Discard, reader, int64(len(body)))
			case "read":
				_, err = reader.Read(buffer)
			}
			if !errors.Is(err, Digest) {
				t.Fatal("tampered final bytes were accepted", err)
			}
			if !bytes.Equal(buffer, make([]byte, len(buffer))) {
				t.Fatal("tampered final bytes retained in caller buffer")
			}
		})
	}
}
func TestReaderRejectsGrowth(t *testing.T) {
	key, pub := signingKey(t)
	files := fixture(t, key, "v1.0.0")
	release, err := verify(t.Context(), files, pub, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := release.Open("oberth")
	if err != nil {
		t.Fatal(err)
	}
	files["oberth"].Data = append(files["oberth"].Data, 'x')
	_, err = reader.Read(make([]byte, len(files["oberth"].Data)))
	closeErr := reader.Close()
	if !errors.Is(err, Digest) {
		t.Fatal("growth was returned without an error on that read", err, closeErr)
	}
}
func TestCleanReleaseRequiresExactEightPayloads(t *testing.T) {
	key, pub := signingKey(t)
	for _, kind := range []string{"valid", "subset", "wrong-name", "extra", "missing-file"} {
		t.Run(kind, func(t *testing.T) {
			files := payloadInventory(t, key, kind)
			release, err := verify(t.Context(), files, pub, "v1.0.0")
			if err != nil {
				t.Fatal(err)
			}
			if err := release.CheckInventory(); (err == nil) != (kind == "valid" || kind == "missing-file") {
				t.Fatal("publisher inventory", kind, err)
			}
			if kind == "missing-file" {
				delete(files, "oberth-linux-amd64")
			}
			err = release.CheckFiles()
			if (err == nil) != (kind == "valid") {
				t.Fatal("exact payload inventory", kind, err)
			}
		})
	}
}

func payloadInventory(t *testing.T, key *ecdsa.PrivateKey, kind string) fstest.MapFS {
	t.Helper()
	files := fixture(t, key, "v1.0.0")
	delete(files, "oberth")
	for _, name := range PayloadNames() {
		if name != "release.json" {
			files[name] = &fstest.MapFile{Data: []byte("payload")}
		}
	}

	switch kind {
	case "subset":
		delete(files, "oberth-linux-amd64")
	case "wrong-name":
		files["other"] = files["oberth-linux-amd64"]
		delete(files, "oberth-linux-amd64")
	case "extra":
		files["other"] = &fstest.MapFile{Data: []byte("extra")}
	}
	signPayloadSet(t, files, key)
	return files
}

func signPayloadSet(t *testing.T, files fstest.MapFS, key *ecdsa.PrivateKey) {
	t.Helper()
	names := make([]string, 0, len(files))
	for name := range files {
		if name != "SHA256SUMS" && name != "SHA256SUMS.sigstore.json" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	var sums strings.Builder
	for _, name := range names {
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(files[name].Data), name)
	}
	files["SHA256SUMS"].Data = []byte(sums.String())
	files["SHA256SUMS.sigstore.json"].Data = signed(t, key, []byte(sums.String()))
}
