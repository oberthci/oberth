package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestOfflineReleaseLookupPrefersVersionDirectoryToSignedFlatRelease(t *testing.T) {
	_, previous := signedInstallRelease(t, "v0.1.0")
	_, target := signedInstallRelease(t, "v0.2.0")
	root := t.TempDir()
	for destination, source := range map[string]string{root: previous, filepath.Join(root, "v0.2.0"): target} {
		if err := os.CopyFS(destination, os.DirFS(source)); err != nil {
			t.Fatal(err)
		}
	}
	r := offlineLookupRequest(t, root, "v0.2.0")
	release, err := installationRelease(t.Context(), r)
	if err != nil || release.Version() != "v0.2.0" || !release.Verified() {
		t.Fatal("flat source hid requested signed version", err)
	}
	if err := os.WriteFile(filepath.Join(root, "v0.2.0", "SHA256SUMS.sigstore.json"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := installationRelease(t.Context(), r); err == nil {
		t.Fatal("invalid selected version fell back to flat source")
	}
	if _, err := installationRelease(t.Context(), offlineLookupRequest(t, root, "v0.1.0")); err != nil {
		t.Fatal("absent version subdirectory did not use verified flat release", err)
	}
}

func offlineLookupRequest(t *testing.T, root, version string) installationRequest {
	t.Helper()
	flags := installationFlags("install")
	if err := flags.Parse([]string{"--version", version, "--release-dir", root}); err != nil {
		t.Fatal(err)
	}
	return installationRequest{name: "install", flags: flags}
}

func TestOfflineReleaseLookupMissingVersionNamesBothDirectories(t *testing.T) {
	for _, versionDirectory := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "incomplete"}[versionDirectory], func(t *testing.T) {
			root := t.TempDir()
			if versionDirectory {
				if err := os.Mkdir(filepath.Join(root, "v0.2.0"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "release.json"), []byte("must not use flat fallback"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := installationRelease(t.Context(), offlineLookupRequest(t, root, "v0.2.0"))
			assertOfflineLookupProblem(t, err, root, "v0.2.0")
		})
	}
}

func assertOfflineLookupProblem(t *testing.T, err error, root, version string) {
	t.Helper()
	var problem *publicerr.Error
	if !errors.As(err, &problem) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("missing version lost its typed refusal or cause", err)
	}
	want := []string{"Required release version: " + version,
		"Searched version directory: " + filepath.Join(root, version), "Searched flat directory: " + root}
	if problem.ID != localReleaseFilesID || !reflect.DeepEqual(problem.Context, want) {
		t.Fatal("required version or search paths missing", problem)
	}
	if len([]rune(problem.Reason())) > 40 || len([]rune(problem.Remedy())) > 120 || strings.Contains(problem.Reason(), root) {
		t.Fatal("lookup refusal puts paths in short reason or exceeds budgets", problem)
	}
}

func TestOfflineRollbackMissingSourceShowsLookupGuidanceWithoutMutation(t *testing.T) {
	f := signedMaintenanceFixture(t)
	upgradeSignedFixture(t, &f)
	app, _, diagnostic := f.rollbackApplication(t)
	root := t.TempDir()
	path := filepath.Join(app.base, "installed", "rollback.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	history, applies := f.charts.History, len(f.charts.Applies)
	if code := app.run(t.Context(), []string{"rollback", "--release-dir", root, "--yes"}); code != 1 {
		t.Fatal("missing source was not refused", code)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) || !reflect.DeepEqual(history, f.charts.History) || len(f.charts.Applies) != applies {
		t.Fatal("missing source changed the operation record or chart", err)
	}
	rendered := strings.ReplaceAll(diagnostic.String(), ":\n", ": ")
	for _, text := range []string{"Required release files were not found.", "Required release version: v0.1.0",
		filepath.Join(root, "v0.1.0"), "Searched flat directory: " + root} {
		if !strings.Contains(rendered, text) {
			t.Fatal("rollback wrapper hid offline guidance", text, diagnostic)
		}
	}
}

func TestOfflineReleaseLookupReadFailureDoesNotFallBack(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "v0.2.0"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := localInstallationReleaseFiles(root, "v0.2.0")
	var problem *publicerr.Error
	if !errors.As(err, &problem) || problem.Reason() != "Release directory cannot be read." {
		t.Fatal("non-directory version hid the storage failure", err)
	}
}
