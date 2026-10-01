package artifacts

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type member struct {
	name     string
	body     string
	typeflag byte
	linkname string
	mode     int64
}

func archiveOf(t *testing.T, members ...member) io.Reader {
	t.Helper()
	var raw bytes.Buffer
	compressor := gzip.NewWriter(&raw)
	writer := tar.NewWriter(compressor)
	for _, entry := range members {
		flag := entry.typeflag
		if flag == 0 {
			flag = tar.TypeReg
		}
		mode := entry.mode
		if mode == 0 {
			mode = 0o644
		}
		header := &tar.Header{
			Name:     entry.name,
			Typeflag: flag,
			Linkname: entry.linkname,
			Mode:     mode,
			Size:     int64(len(entry.body)),
		}
		if flag != tar.TypeReg {
			header.Size = 0
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if flag == tar.TypeReg {
			if _, err := writer.Write([]byte(entry.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressor.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(raw.Bytes())
}

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "artifacts")
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	return store, root
}

func TestExtractStoresRegularFiles(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	archive := archiveOf(t,
		member{name: "surefire/TEST-a.xml", body: "<testsuite/>"},
		member{name: "coverage/index.html", body: "<html></html>"},
	)
	manifest, err := store.Extract("run-abc", archive, 1<<20, DefaultScanPatterns)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(manifest.Entries) != 2 {
		t.Fatalf("stored %d entries, want 2: %+v", len(manifest.Entries), manifest.Entries)
	}
	body, err := store.ReadAll("run-abc", "surefire/TEST-a.xml")
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(body) != "<testsuite/>" {
		t.Fatalf("stored %q", body)
	}
}

func TestExtractRefusesHostileMembers(t *testing.T) {
	t.Parallel()
	hostile := map[string]member{
		"absolute path":        {name: "/etc/cron.d/pwned", body: "x"},
		"parent traversal":     {name: "../../escape", body: "x"},
		"embedded traversal":   {name: "reports/../../escape", body: "x"},
		"symlink":              {name: "link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
		"symlink to parent":    {name: "link", typeflag: tar.TypeSymlink, linkname: "../../"},
		"hardlink":             {name: "hard", typeflag: tar.TypeLink, linkname: "/etc/passwd"},
		"character device":     {name: "dev", typeflag: tar.TypeChar},
		"block device":         {name: "blk", typeflag: tar.TypeBlock},
		"fifo":                 {name: "pipe", typeflag: tar.TypeFifo},
		"empty name":           {name: "", body: "x"},
		"current directory":    {name: ".", body: "x"},
		"dot dot alone":        {name: "..", body: "x"},
		"backslash traversal":  {name: `..\..\escape`, body: "x"},
		"leading dot segment":  {name: "./../escape", body: "x"},
		"trailing dot segment": {name: "reports/..", body: "x"},
	}
	for label, entry := range hostile {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			store, root := newStore(t)
			_, err := store.Extract("run-abc", archiveOf(t, entry), 1<<20, DefaultScanPatterns)
			if err == nil {
				t.Fatalf("Extract admitted %q", entry.name)
			}
			if !errors.Is(err, ErrRefused) {
				t.Fatalf("Extract failed for an incidental reason rather than refusing %q: %v", entry.name, err)
			}
			assertNothingWritten(t, root)
		})
	}
}

func TestSafeMemberNameRefusesWhatTheTarWriterCannotEncode(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"bad\x00name",
		"bad\nname",
		"bad\rname",
		`..\..\escape`,
		"/absolute",
		"../escape",
		"",
		".",
		"..",
	} {
		if cleaned, err := safeMemberName(raw); err == nil {
			t.Fatalf("safeMemberName(%q) admitted it as %q", raw, cleaned)
		}
	}
	for _, raw := range []string{"a.txt", "reports/a.txt", "./reports/a.txt", "a/b/c.xml"} {
		if _, err := safeMemberName(raw); err != nil {
			t.Fatalf("safeMemberName(%q) refused a legitimate name: %v", raw, err)
		}
	}
}

func TestExtractWritesNothingWhenALaterMemberIsHostile(t *testing.T) {
	t.Parallel()
	store, root := newStore(t)
	archive := archiveOf(t,
		member{name: "good/report.xml", body: "fine"},
		member{name: "../escape", body: "bad"},
	)
	if _, err := store.Extract("run-abc", archive, 1<<20, DefaultScanPatterns); err == nil {
		t.Fatal("Extract admitted an archive whose second member escapes")
	}
	assertNothingWritten(t, root)
}

func assertNothingWritten(t *testing.T, root string) {
	t.Helper()
	var found []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil //nolint:nilerr
		}
		found = append(found, path)
		return nil
	})
	if len(found) != 0 {
		t.Fatalf("a refused archive left files behind: %v", found)
	}
}

func TestExtractRefusesAnArchiveOverTheLimit(t *testing.T) {
	t.Parallel()
	store, root := newStore(t)
	archive := archiveOf(t, member{name: "big", body: strings.Repeat("x", 5000)})
	_, err := store.Extract("run-abc", archive, 1000, DefaultScanPatterns)
	if err == nil {
		t.Fatal("Extract admitted an archive over the caller's limit")
	}
	if !strings.Contains(err.Error(), "1000") {
		t.Fatalf("error does not name the limit it enforced: %v", err)
	}
	assertNothingWritten(t, root)
}

func TestExtractLimitsDecompressedBytesNotArchiveBytes(t *testing.T) {
	t.Parallel()
	store, root := newStore(t)
	archive := archiveOf(t, member{name: "bomb", body: strings.Repeat("a", 4<<20)})
	compressed, err := io.ReadAll(archive)
	if err != nil {
		t.Fatal(err)
	}
	if len(compressed) >= 1<<20 {
		t.Fatalf("fixture compressed to %d bytes, too big to prove the point", len(compressed))
	}
	_, err = store.Extract("run-abc", bytes.NewReader(compressed), 1<<20, DefaultScanPatterns)
	if err == nil {
		t.Fatalf("a %d byte archive expanding to 4 MiB was admitted against a 1 MiB limit", len(compressed))
	}
	assertNothingWritten(t, root)
}

func TestExtractRefusesAnInvalidRunID(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	for _, runID := range []string{"", "../escape", "/absolute", ".hidden", strings.Repeat("x", 81)} {
		if _, err := store.Extract(runID, archiveOf(t, member{name: "a", body: "b"}), 1<<20, DefaultScanPatterns); err == nil {
			t.Fatalf("Extract admitted run ID %q", runID)
		}
	}
}

func TestExtractReplacesAnEarlierCollection(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	if _, err := store.Extract("run-abc", archiveOf(t, member{name: "first", body: "1"}), 1<<20, DefaultScanPatterns); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Extract("run-abc", archiveOf(t, member{name: "second", body: "2"}), 1<<20, DefaultScanPatterns); err != nil {
		t.Fatal(err)
	}
	entries, err := store.List("run-abc")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "second" {
		t.Fatalf("a second collection did not replace the first: %+v", entries)
	}
}

func TestListReportsNameSizeAndTime(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	if _, err := store.Extract("run-abc", archiveOf(t,
		member{name: "b/second.txt", body: "22"},
		member{name: "a/first.txt", body: "1"},
	), 1<<20, DefaultScanPatterns); err != nil {
		t.Fatal(err)
	}
	entries, err := store.List("run-abc")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("listed %d entries, want 2", len(entries))
	}
	if entries[0].Name != "a/first.txt" || entries[1].Name != "b/second.txt" {
		t.Fatalf("entries are not in a stable sorted order: %+v", entries)
	}
	if entries[0].Size != 1 || entries[1].Size != 2 {
		t.Fatalf("sizes wrong: %+v", entries)
	}
	if entries[0].Modified.IsZero() {
		t.Fatal("no modification time recorded")
	}
}

func TestListOfAnUncollectedRunIsEmptyNotAnError(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	entries, err := store.List("run-never")
	if err != nil {
		t.Fatalf("List of a run with no artifacts: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("listed %d entries for a run that collected nothing", len(entries))
	}
}

func TestReadRefusesTraversalOnTheReadSide(t *testing.T) {
	t.Parallel()
	store, root := newStore(t)
	if _, err := store.Extract("run-abc", archiveOf(t, member{name: "ok.txt", body: "fine"}), 1<<20, DefaultScanPatterns); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(filepath.Dir(root), "outside.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"../outside.txt",
		"../../outside.txt",
		"/etc/passwd",
		"",
		".",
		"ok.txt/../../outside.txt",
	} {
		if _, err := store.ReadAll("run-abc", name); err == nil {
			t.Fatalf("ReadAll admitted %q", name)
		}
	}
}

func TestExtractRefusesAnEmptyArchiveWithoutCreatingARunDirectory(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	manifest, err := store.Extract("run-abc", archiveOf(t), 1<<20, DefaultScanPatterns)
	if err != nil {
		t.Fatalf("an empty archive should collect nothing, not fail: %v", err)
	}
	if len(manifest.Entries) != 0 {
		t.Fatalf("empty archive produced %d entries", len(manifest.Entries))
	}
	entries, err := store.List("run-abc")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("empty archive listed %d entries", len(entries))
	}
}

func FuzzExtract(f *testing.F) {
	f.Add([]byte("not a gzip stream"))
	var seed bytes.Buffer
	compressor := gzip.NewWriter(&seed)
	writer := tar.NewWriter(compressor)
	_ = writer.WriteHeader(&tar.Header{Name: "a", Mode: 0o644, Size: 1})
	_, _ = writer.Write([]byte("x"))
	_ = writer.Close()
	_ = compressor.Close()
	f.Add(seed.Bytes())

	f.Fuzz(func(t *testing.T, raw []byte) {
		store, err := Open(filepath.Join(t.TempDir(), "artifacts"))
		if err != nil {
			t.Skip()
		}
		_, _ = store.Extract("run-fuzz", bytes.NewReader(raw), 1<<16, DefaultScanPatterns)
	})
}

func TestExtractAcceptsAnArchiveWrittenByTarDashC(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	archive := archiveOf(t,
		member{name: "./", typeflag: tar.TypeDir},
		member{name: "./surefire/", typeflag: tar.TypeDir},
		member{name: "./surefire/TEST-Report.xml", body: "<testsuite/>"},
	)
	manifest, err := store.Extract("run-abc", archive, 1<<20, DefaultScanPatterns)
	if err != nil {
		t.Fatalf("the shape `tar -czf - -C dir .` actually produces was refused: %v", err)
	}
	if len(manifest.Entries) != 1 || manifest.Entries[0].Name != "surefire/TEST-Report.xml" {
		t.Fatalf("entries = %+v", manifest.Entries)
	}
}

func TestExtractRefusesSecretMaterialBeforeWriting(t *testing.T) {
	t.Parallel()
	store, err := Open(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	archive := archiveOf(t,
		member{name: "report.txt", body: "all tests passed"},
		member{name: "debug/dump.txt", body: "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaA==\n-----END OPENSSH PRIVATE KEY-----"},
	)

	_, err = store.Extract("run-abc", archive, 1<<20, DefaultScanPatterns)
	if !errors.Is(err, ErrSecretDetected) {
		t.Fatalf("secret material was not refused: %v", err)
	}
	if !strings.Contains(err.Error(), "debug/dump.txt") {
		t.Fatalf("refusal does not name the member: %v", err)
	}
	if strings.Contains(err.Error(), "b3BlbnNzaA") {
		t.Fatalf("refusal leaks matched content: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(store.directory, "run-abc")); !os.IsNotExist(statErr) {
		t.Fatal("a refused collection left files on disk; the scan must run before any write")
	}
}

func TestDefaultScanPatternsCatchAnEmbeddedServiceAccountKey(t *testing.T) {
	t.Parallel()
	store, err := Open(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	// A GCP service-account JSON is one long line embedding the PEM header
	// verbatim with escaped newlines — the line-oriented scan must still see it.
	serviceAccount := `{"type":"service_account","private_key":"-----BEGIN PRIVATE KEY-----\nMIIEvA==\n-----END PRIVATE KEY-----\n","client_email":"x@y.iam.gserviceaccount.com"}`
	archive := archiveOf(t, member{name: "ci-env.json", body: serviceAccount})

	if _, err := store.Extract("run-abc", archive, 1<<20, DefaultScanPatterns); !errors.Is(err, ErrSecretDetected) {
		t.Fatalf("embedded service-account key was not refused: %v", err)
	}
}

func TestExtractWithoutPatternsStillExtracts(t *testing.T) {
	t.Parallel()
	// nil patterns disable the scan (the redact package's documented empty-list
	// contract); the persisting call site always passes DefaultScanPatterns —
	// this exists to pin the empty-list semantics rather than to encourage it.
	store, err := Open(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Extract("run-abc", archiveOf(t, member{name: "a.txt", body: "clean"}), 1<<20, nil); err != nil {
		t.Fatalf("nil-pattern extract failed: %v", err)
	}
}
