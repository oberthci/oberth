package installer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestUpgradeRequiresSupportedHelm(t *testing.T) {
	for _, version := range []string{"v3.13.3", "v3.14.0-rc.1", "v3.14.0", "v3.18.6+gfixture", "v4.2.3", "", "unparseable"} {
		t.Run(version, func(t *testing.T) {
			calls := 0
			deps := Deps{RunHelm: func(_ context.Context, args []string) ([]byte, error) {
				calls++
				if !slices.Equal(args, []string{"version", "--template", "{{.Version}}"}) {
					t.Fatal(args)
				}
				return []byte(version), nil
			}}
			err := requireUpgradeHelmVersion(t.Context(), deps)
			supported := version == "v3.14.0" || version == "v3.18.6+gfixture" || version == "v4.2.3"
			if (err == nil) != supported || calls != 1 {
				t.Fatalf("version=%s err=%v calls=%d", version, err, calls)
			}
			if err != nil && !strings.Contains(err.Error(), "Helm >= 3.14.0") {
				t.Fatal(err)
			}
		})
	}
	want := errors.New("Helm unavailable")
	if err := requireUpgradeHelmVersion(t.Context(), Deps{RunHelm: func(context.Context, []string) ([]byte, error) { return nil, want }}); !errors.Is(err, want) {
		t.Fatal(err)
	}
}

func TestUpgradeCompatibilityNeverFallsBackToNewDefaults(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `invalid`, `{"compatibility":null}`, `{"compatibility":{"schemaIdentity":"old","auditDomain":"old"}}`, `{"compatibility":{"schemaIdentity":5,"auditDomain":"old","witnessKeyInfo":"old"}}`, `{"compatibility":{"schemaIdentity":"old","auditDomain":"bad\nvalue","witnessKeyInfo":"old"}}`} {
		deps := Deps{RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if !slices.Contains(args, "--all") {
				t.Fatal("identities must include old defaults")
			}
			return []byte(raw), nil
		}}
		if _, err := readUpgradeCompatibility(t.Context(), "chosen", deps); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	failure := errors.New("cannot read current release")
	_, err := readUpgradeCompatibility(t.Context(), "chosen", Deps{RunHelm: func(context.Context, []string) ([]byte, error) { return nil, failure }})
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
}

func TestUpgradeCompatibilityOverridesArePinned(t *testing.T) {
	for _, key := range []string{"compatibility", "compatibility.schemaIdentity", "compatibility.auditDomain", "compatibility.witnessKeyInfo"} {
		if err := checkPinnedKeyConflicts([]string{key + "=foreign"}, nil); err == nil {
			t.Fatalf("identity override accepted: %s", key)
		}
	}
	for _, body := range []string{"compatibility: null\n", "compatibility:\n  schemaIdentity: foreign\n", "compatibility:\n  auditDomain: foreign\n", "compatibility:\n  witnessKeyInfo: foreign\n"} {
		path := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := checkPinnedKeyConflicts(nil, []string{path}); err == nil {
			t.Fatalf("identity values override accepted: %s", body)
		}
	}
}

func TestUpgradeSchemaPreflightUsesOnlyStoredOverridesAndCleansFiles(t *testing.T) {
	for _, failAt := range []string{"", "stored-read", "stored-json", "schema"} {
		t.Run(failAt, func(t *testing.T) {
			calls := []string{}
			temporary := ""
			deps := Deps{RunHelm: func(_ context.Context, args []string) ([]byte, error) {
				calls = append(calls, args[0])
				if args[0] == "get" {
					if slices.Contains(args, "--all") {
						raw, _ := testUpgradeValuePreflight(args)
						return raw, nil
					}
					if failAt == "stored-read" {
						return nil, errors.New("read failed")
					}
					if failAt == "stored-json" {
						return []byte("invalid"), nil
					}
					return []byte(`{"pushBannerURL":"","retention":{"keepNewestPerRef":0},"podLabels":{"arbitrary.example/key":"kept"}}`), nil
				}
				if args[0] != "template" {
					t.Fatalf("preflight mutated release: %v", args)
				}
				if slices.Contains(args, "--wait") || slices.Contains(args, "--timeout") || slices.Contains(args, "--reset-then-reuse-values") {
					t.Fatal(args)
				}
				if !slices.Contains(args, "--is-upgrade") || !slices.Contains(args, "compatibility.schemaIdentity="+testUpgradeCompatibility.SchemaIdentity) || !slices.Contains(args, "watchTunnel.image="+watchTunnelImageDefault) {
					t.Fatal(args)
				}
				temporary = args[slices.Index(args, "--values")+1]
				info, err := os.Stat(temporary)
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatalf("unsafe snapshot: %v %v", info, err)
				}
				body, err := os.ReadFile(temporary)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(body), "compatibility") || !strings.Contains(string(body), "keepNewestPerRef") {
					t.Fatalf("snapshot must contain user overrides, not old defaults: %s", body)
				}
				if failAt == "schema" {
					return nil, errors.New("additional properties removedKey not allowed")
				}
				return nil, nil
			}}
			compat, err := prepareUpgradeValues(t.Context(), UpgradeConfig{Namespace: "chosen"}, deps, "chart", "image")
			if (err == nil) != (failAt == "") {
				t.Fatalf("%s: %v", failAt, err)
			}
			if err == nil && compat != testUpgradeCompatibility {
				t.Fatal(compat)
			}
			if temporary != "" {
				if _, err := os.Stat(filepath.Dir(temporary)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("snapshot leaked: %v", err)
				}
			}
			if len(calls) < 2 || calls[0] != "get" || calls[1] != "get" {
				t.Fatal(calls)
			}
		})
	}
}
