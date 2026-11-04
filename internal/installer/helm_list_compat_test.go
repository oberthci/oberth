package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Model Helm's status selection rather than returning a release regardless
// of the requested filters. Helm 4 rejects the removed all-status flag.
func helmStatusListStub(t *testing.T, major int, status string) func(context.Context, []string) ([]byte, error) {
	t.Helper()
	return func(_ context.Context, args []string) ([]byte, error) {
		if len(args) == 0 || args[0] != "list" {
			t.Fatalf("unexpected Helm command: %v", args)
		}
		if !slices.Contains(args, "-n") || !slices.Contains(args, "oberth") || !slices.Contains(args, "json") {
			t.Fatalf("release lookup lost namespace or JSON output: %v", args)
		}
		all := slices.Contains(args, "-a") || slices.Contains(args, "--all")
		if major == 4 && all {
			return nil, errors.New("unknown shorthand flag: 'a' in -a")
		}
		filter := "--" + status
		if strings.HasPrefix(status, "pending-") {
			filter = "--pending"
		}
		if status == "" || (!all && !slices.Contains(args, filter)) {
			return []byte(`[]`), nil
		}
		return json.Marshal([]helmRelease{{Name: "oberth", Namespace: "oberth", Status: status, Chart: "oberth-0.17.28"}})
	}
}

func TestHelmReleaseLookupCompatibility(t *testing.T) {
	for _, major := range []int{3, 4} {
		for _, status := range []string{"deployed", "failed", "pending-install", "pending-upgrade", "pending-rollback", "superseded", "uninstalling", "uninstalled", ""} {
			t.Run(fmt.Sprintf("Helm%d/%s", major, status), func(t *testing.T) {
				deps := Deps{RunHelm: helmStatusListStub(t, major, status)}
				release, found, err := findHelmRelease(t.Context(), deps, "oberth", "oberth")
				if err != nil || found != (status != "") || release.Status != status {
					t.Fatalf("release=%+v found=%v err=%v", release, found, err)
				}
			})
		}
	}
}

func TestBackupHelmCompatibility(t *testing.T) {
	for _, major := range []int{3, 4} {
		for _, scope := range []string{BackupScopeHistory, BackupScopeFull} {
			t.Run(fmt.Sprintf("Helm%d/%s", major, scope), func(t *testing.T) {
				source := newFakeBackupCluster(t, sourceCompat)
				source.database, source.rawFiles, _ = sourceDatabase(t)
				var out bytes.Buffer
				deps := source.deps(&out)
				list := helmStatusListStub(t, major, "deployed")
				deps.RunHelm = func(ctx context.Context, args []string) ([]byte, error) {
					if args[0] == "list" {
						return list(ctx, args)
					}
					return source.helm(ctx, args)
				}
				if scope == BackupScopeHistory {
					deps.rootToken = ""
				}
				cfg := BackupConfig{Scope: scope, Output: filepath.Join(t.TempDir(), "backup.cms"), Recipient: "recipient.pem"}
				cfg.applyDefaults()
				if err := runBackup(t.Context(), cfg, deps); err != nil {
					t.Fatal(err)
				}
				manifest, err := openBackupArchive(t.Context(), cfg.Output, fakeEnvelopeCipher{}, t.TempDir(), 0)
				if err != nil || manifest.Scope != scope || manifest.Source.ChartVersion != "0.17.28" {
					t.Fatalf("manifest=%+v err=%v", manifest, err)
				}
				if scope == BackupScopeHistory {
					if len(manifest.Members) != 1 || manifest.Members[0].Name != backupMemberDatabase {
						t.Fatalf("history archive members=%v", manifest.Members)
					}
					for _, args := range source.argvs {
						if slices.Contains(args, "openbao-0") {
							t.Fatal("history backup contacted OpenBao")
						}
					}
				}
			})
		}
	}
}

func TestHelmListingFailuresStopBackupAndRevisionCheck(t *testing.T) {
	listingErr := errors.New("release listing denied")
	for _, mode := range []string{"transport", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			list := func(context.Context, []string) ([]byte, error) {
				if mode == "transport" {
					return []byte(`[]`), listingErr
				}
				return []byte(`not JSON`), nil
			}
			if err := detectFailedHelmRevision(t.Context(), Deps{RunHelm: list}, "oberth"); err == nil || (mode == "transport" && !errors.Is(err, listingErr)) {
				t.Errorf("failed-revision check ignored listing failure: %v", err)
			}
			for _, scope := range []string{BackupScopeHistory, BackupScopeFull} {
				source := newFakeBackupCluster(t, sourceCompat)
				var out bytes.Buffer
				deps := source.deps(&out)
				deps.RunHelm = list
				cfg := BackupConfig{Scope: scope, Output: filepath.Join(t.TempDir(), "backup.cms"), Recipient: "recipient.pem"}
				cfg.applyDefaults()
				err := runBackup(t.Context(), cfg, deps)
				if err == nil || (mode == "transport" && !errors.Is(err, listingErr)) {
					t.Fatalf("%s backup ignored listing failure: %v", scope, err)
				}
				if len(source.argvs) != 0 {
					t.Fatalf("backup ran commands after listing failure: %v", source.argvs)
				}
				if _, err := os.Stat(cfg.Output); !os.IsNotExist(err) {
					t.Fatal("failed listing created a backup archive")
				}
			}
		})
	}
}

func TestHelmCompatibilityForInstallSafetyChecks(t *testing.T) {
	for _, major := range []int{3, 4} {
		t.Run(fmt.Sprintf("Helm%d", major), func(t *testing.T) {
			list := helmStatusListStub(t, major, "failed")
			deps := Deps{RunHelm: func(ctx context.Context, args []string) ([]byte, error) {
				if args[0] == "list" {
					return list(ctx, args)
				}
				if args[0] != "get" || args[1] != "values" {
					t.Fatalf("unexpected Helm command: %v", args)
				}
				return []byte(`{"secretstore":{"address":"https://bao.example"},"tls":{"pki":{"enabled":true}}}`), nil
			}}
			if err := detectFailedHelmRevision(t.Context(), deps, "oberth"); err == nil || !strings.Contains(err.Error(), "previous deployed revision") {
				t.Fatalf("failed release did not reach revision safety check: %v", err)
			}
			cfg := Config{Namespace: "oberth"}
			if err := promptSecretStoreChoice(t.Context(), &cfg, deps); err != nil || !cfg.serverPKI {
				t.Fatalf("existing secret-store configuration lost: err=%v PKI=%v", err, cfg.serverPKI)
			}
		})
	}
}
