package releaseverify

import (
	"crypto/ecdsa"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

type sizedInfo struct {
	fs.FileInfo
	size int64
}

func (i sizedInfo) Size() int64 { return i.size }

type sizedFile struct {
	fs.File
	size int64
}

func (f sizedFile) Stat() (fs.FileInfo, error) {
	info, err := f.File.Stat()
	return sizedInfo{info, f.size}, err
}

type sizedFS struct {
	fstest.MapFS
	name string
	size int64
	deny bool
}

func (f sizedFS) Open(name string) (fs.File, error) {
	if name == f.name && f.deny {
		return nil, fs.ErrPermission
	}
	file, err := f.MapFS.Open(name)
	if err == nil && name == f.name {
		return sizedFile{file, f.size}, nil
	}
	return file, err
}
func (f sizedFS) Lstat(name string) (fs.FileInfo, error) {
	info, err := fs.Stat(f.MapFS, name)
	if err == nil && name == f.name {
		return sizedInfo{info, f.size}, nil
	}
	return info, err
}
func (f sizedFS) ReadLink(string) (string, error) { return "", fs.ErrInvalid }
func TestArtifactLimitsAndLengths(t *testing.T) {
	key, pub := signingKey(t)
	for _, name := range []string{"oberth", "oberth.tgz"} {
		max := int64(MaxArtifact)
		if name == "oberth.tgz" {
			max = 64 << 20
		}
		for _, size := range []int64{max, max + 1} {
			checkArtifactSize(t, sizedFS{MapFS: fixture(t, key, "v1.0.0"), name: name, size: size}, key, pub)
		}
	}
}
func checkArtifactSize(t *testing.T, f sizedFS, key *ecdsa.PrivateKey, pub []byte) {
	t.Helper()
	max := int64(MaxArtifact)
	if f.name == "oberth.tgz" {
		max = 64 << 20
		f.MapFS[f.name] = f.MapFS["oberth"]
		delete(f.MapFS, "oberth")
		f.MapFS["SHA256SUMS"].Data = []byte(strings.Replace(string(f.MapFS["SHA256SUMS"].Data), "  oberth\n", "  oberth.tgz\n", 1))
		f.MapFS["SHA256SUMS.sigstore.json"].Data = signed(t, key, f.MapFS["SHA256SUMS"].Data)
	}
	release, err := verify(t.Context(), f, pub, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := release.Open(f.name)
	if f.size > max {
		if !errors.Is(err, Limit) {
			t.Fatal("over limit accepted", err)
		}
		return
	}
	if err != nil {
		t.Fatal("limit refused", err)
	}
	if err := reader.Close(); !errors.Is(err, Digest) {
		t.Fatal("wrong length accepted", err)
	}

}
func TestReadFileCallerBound(t *testing.T) {
	key, pub := signingKey(t)
	f := fixture(t, key, "v1.0.0")
	release, err := verify(t.Context(), f, pub, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	size := int64(len(f["oberth"].Data))
	if _, err := release.ReadFile("oberth", size); err != nil {
		t.Fatal(err)
	}
	if _, err := release.ReadFile("oberth", size-1); !errors.Is(err, Limit) {
		t.Fatal("caller bound ignored", err)
	}
}
func TestArtifactReadFailureIsNotDigestFailure(t *testing.T) {
	key, pub := signingKey(t)
	f := fixture(t, key, "v1.0.0")
	release, err := verify(t.Context(), sizedFS{MapFS: f, name: "oberth", size: 12, deny: true}, pub, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	_, err = release.Open("oberth")
	detail, ok := err.(interface{ Detail() string })
	if !ok || !strings.Contains(detail.Detail(), fs.ErrPermission.Error()) || err.Error() != Read.Error() {
		t.Fatal("reason and detail were mixed", err)
	}
	if !errors.Is(err, Read) || !errors.Is(err, fs.ErrPermission) || errors.Is(err, Digest) {
		t.Fatal("read error lost", err)
	}
}

func TestUnknownLengthArtifactBoundAndDigest(t *testing.T) {
	key, pub := signingKey(t)
	for _, kind := range []string{"valid", "corrupt", "over-bound"} {
		t.Run(kind, func(t *testing.T) { checkUnknownArtifact(t, key, pub, kind) })
	}
}

func checkUnknownArtifact(t *testing.T, key *ecdsa.PrivateKey, pub []byte, kind string) {
	t.Helper()
	files := fixture(t, key, "v1.0.0")
	release, err := verify(t.Context(), sizedFS{MapFS: files, name: "oberth", size: -1}, pub, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if kind == "corrupt" {
		files["oberth"].Data[0] ^= 1
	}
	reader, err := release.Open("oberth")
	if err != nil {
		t.Fatal(err)
	}
	if kind == "over-bound" {
		reader.(*checkedReader).max = 1
	}
	err = reader.Close()
	want := error(nil)
	if kind == "corrupt" {
		want = Digest
	}
	if kind == "over-bound" {
		want = Limit
	}
	if !errors.Is(err, want) {
		t.Fatal("wrong stream result", err)
	}
}
