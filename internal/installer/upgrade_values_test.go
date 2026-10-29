package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
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

func TestUpgradeDeployedRevision(t *testing.T) {
	for _, test := range []struct {
		name, history, want string
	}{
		{"highest-deployed-not-latest", `[{"revision":5,"status":"failed"},{"revision":4,"status":"deployed"},{"revision":2,"status":"deployed"}]`, "4"},
		{"invalid-json", `invalid`, ""},
		{"no-deployed", `[{"revision":5,"status":"failed"}]`, ""},
		{"invalid-revision", `[{"revision":0,"status":"deployed"}]`, ""},
		{"empty", `[]`, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			deps := Deps{RunHelm: func(_ context.Context, args []string) ([]byte, error) {
				if !slices.Equal(args, []string{"history", "oberth", "-n", "chosen", "-o", "json"}) {
					t.Fatal(args)
				}
				return []byte(test.history), nil
			}}
			got, err := deployedUpgradeRevision(t.Context(), "chosen", deps)
			if got != test.want || (err == nil) != (test.want != "") {
				t.Fatalf("revision = %q, err = %v; want %q", got, err, test.want)
			}
		})
	}
	failure := errors.New("history unavailable")
	_, err := deployedUpgradeRevision(t.Context(), "chosen", Deps{RunHelm: func(context.Context, []string) ([]byte, error) { return nil, failure }})
	if !errors.Is(err, failure) {
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
		if _, err := readUpgradeCompatibility(t.Context(), "chosen", "1", deps); err == nil {
			t.Fatalf("accepted %s", raw)
		} else if raw == `{}` && (!strings.Contains(err.Error(), "older than v0.17.5") || !strings.Contains(err.Error(), "existing database and witness configuration")) {
			t.Fatalf("missing legacy-release recovery hint: %v", err)
		}
	}
	failure := errors.New("cannot read current release")
	_, err := readUpgradeCompatibility(t.Context(), "chosen", "1", Deps{RunHelm: func(context.Context, []string) ([]byte, error) { return nil, failure }})
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
				if args[0] == "history" {
					out, _ := testUpgradeValuePreflight(args)
					return out, nil
				}
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
			prepared, err := prepareUpgradeValues(t.Context(), UpgradeConfig{Namespace: "chosen"}, deps, "chart", "image")
			if (err == nil) != (failAt == "") {
				t.Fatalf("%s: %v", failAt, err)
			}
			if err == nil {
				if prepared.compatibility != testUpgradeCompatibility {
					t.Fatal(prepared.compatibility)
				}
				prepared.cleanup()
			}
			if temporary != "" {
				if _, err := os.Stat(filepath.Dir(temporary)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("snapshot leaked: %v", err)
				}
			}
			if len(calls) < 3 || calls[0] != "history" || calls[1] != "get" || calls[2] != "get" {
				t.Fatal(calls)
			}
		})
	}
}

func TestUpgradeMigratesOnlyStoredBooleanShadow(t *testing.T) {
	for _, raw := range []string{
		`{"gitcache":{"shadow":true},"pushBannerURL":"","retention":{"keepNewestPerRef":0},"argo":{"enabled":false}}`,
		`{"gitcache":{"shadow":false},"podLabels":{"arbitrary.example/key":"kept"},"retention":{"keepNewestPerRef":9007199254740993}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			var output bytes.Buffer
			deps := Deps{Output: &output, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
				if args[0] == "history" {
					out, _ := testUpgradeValuePreflight(args)
					return out, nil
				}
				if args[0] == "get" {
					if slices.Contains(args, "--all") {
						out, _ := testUpgradeValuePreflight(args)
						return out, nil
					}
					return []byte(raw), nil
				}
				if args[0] != "template" {
					t.Fatalf("preflight mutated release: %v", args)
				}
				body, err := os.ReadFile(args[slices.Index(args, "--values")+1])
				if err != nil {
					t.Fatal(err)
				}
				var got, want map[string]any
				gotDecoder := json.NewDecoder(bytes.NewReader(body))
				gotDecoder.UseNumber()
				if err := gotDecoder.Decode(&got); err != nil {
					t.Fatal(err)
				}
				decoder := json.NewDecoder(strings.NewReader(raw))
				decoder.UseNumber()
				if err := decoder.Decode(&want); err != nil {
					t.Fatal(err)
				}
				delete(want, "gitcache")
				gotJSON, _ := json.Marshal(got)
				wantJSON, _ := json.Marshal(want)
				if !bytes.Equal(gotJSON, wantJSON) {
					t.Fatalf("migration changed other stored values: got %s want %s", gotJSON, wantJSON)
				}
				return nil, nil
			}}
			cfg := UpgradeConfig{Namespace: "chosen"}
			prepared, err := prepareUpgradeValues(t.Context(), cfg, deps, "chart", "image")
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.cleanup()
			if prepared.storedValues == "" || !strings.Contains(output.String(), "Dropped stored inert legacy gitcache.shadow") {
				t.Fatal("migration snapshot or explicit deviation missing")
			}
			cfg.storedValues = prepared.storedValues
			_, flags := upgradeHelmArgs(cfg, "chart", "image", prepared.compatibility)
			if !slices.Contains(flags, "--reset-values") || slices.Contains(flags, "--reset-then-reuse-values") || flags[slices.Index(flags, "--values")+1] != prepared.storedValues {
				t.Fatalf("apply would resurrect stored shadow: %v", flags)
			}
			if _, err := os.Stat(prepared.storedValues); err != nil {
				t.Fatalf("snapshot removed before apply: %v", err)
			}
			prepared.cleanup()
			if _, err := os.Stat(prepared.storedValues); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("snapshot leaked: %v", err)
			}
		})
	}
}

// Stop at the Helm boundary: the real command path must apply the exact
// validated migration snapshot, then erase it even if applying fails.
func TestRunUpgradeRetriesFailedShadowMigration(t *testing.T) {
	for name, dryRun := range map[string]bool{"apply": false, "dry-run": true} {
		t.Run(name, func(t *testing.T) {
			stop := errors.New("stop at Helm apply")
			var output bytes.Buffer
			reads, histories, applies := 0, 0, 0
			snapshot := ""
			deps := Deps{
				Output:     &output,
				RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"},
				RunHelm: func(_ context.Context, args []string) ([]byte, error) {
					switch args[0] {
					case "list":
						return []byte(`[{"name":"oberth","revision":"8","status":"failed","chart":"oberth-0.17.63"}]`), nil
					case "history":
						histories++
						return []byte(`[{"revision":7,"status":"deployed"},{"revision":8,"status":"failed"}]`), nil
					case "get":
						reads++
						i := slices.Index(args, "--revision")
						if i < 0 {
							// The latest failed attempt already has a cleaned Config.
							return []byte(`{}`), nil
						}
						if args[i+1] != "7" {
							t.Fatalf("read values from wrong revision: %v", args)
						}
						if slices.Contains(args, "--all") {
							raw, _ := testUpgradeValuePreflight(args)
							return raw, nil
						}
						return []byte(`{"gitcache":{"shadow":true}}`), nil
					case "show":
						return []byte("image:\n  ref: example.invalid/oberth@sha256:" + strings.Repeat("1", 64) + "\n"), nil
					case "template":
						snapshot = args[slices.Index(args, "--values")+1]
						body, err := os.ReadFile(snapshot)
						if err != nil || string(body) != `{}` {
							t.Fatalf("unclean preflight snapshot %s: %v", body, err)
						}
						return nil, nil
					case "upgrade":
						applies++
						if dryRun || !slices.Contains(args, "--reset-values") || slices.Contains(args, "--reset-then-reuse-values") || args[slices.Index(args, "--values")+1] != snapshot {
							t.Fatalf("retry would reuse deployed shadow: %v", args)
						}
						return nil, stop
					default:
						if raw, ok := testUpgradeValuePreflight(args); ok {
							return raw, nil
						}
						t.Fatalf("unexpected Helm call: %v", args)
						return nil, nil
					}
				},
			}
			result, err := RunUpgrade(t.Context(), UpgradeConfig{Namespace: "oberth", BinaryVersion: "0.17.63", ChartOverride: "fixture", DryRun: dryRun}, deps)
			if histories != 1 || result.AlreadyUpToDate || result.Upgraded || result.Migrated != "gitcache.shadow=true" {
				t.Fatalf("retry result=%+v histories=%d err=%v", result, histories, err)
			}
			if dryRun {
				if err != nil || reads != 1 || applies != 0 || !strings.Contains(output.String(), "Would strip stored legacy gitcache.shadow=true from deployed revision 7") || !strings.Contains(output.String(), "--reset-values --values <cleaned-deployed-values.json>") || strings.Contains(output.String(), "--reset-then-reuse-values") {
					t.Fatalf("incorrect migration preview: %s; reads=%d applies=%d err=%v", output.String(), reads, applies, err)
				}
			} else {
				if !errors.Is(err, stop) || reads != 2 || applies != 1 {
					t.Fatalf("retry did not apply migration: reads=%d applies=%d err=%v", reads, applies, err)
				}
				if _, err := os.Stat(snapshot); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("retry leaked snapshot: %v", err)
				}
			}
		})
	}
}

// The exact snapshot validated by preflight must also reach Helm apply.
func TestRunUpgradeAppliesValidatedShadowMigrationSnapshot(t *testing.T) {
	stop := errors.New("stop before applying release")
	var validated []byte
	var snapshot string
	applied := false
	deps := Deps{
		RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"},
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if args[0] == "get" && !slices.Contains(args, "--all") {
				return []byte(`{"gitcache":{"shadow":true},"pushBannerURL":""}`), nil
			}
			if out, ok := testUpgradeValuePreflight(args); ok && args[0] != "template" {
				return out, nil
			}
			switch args[0] {
			case "list":
				return []byte(`[{"name":"oberth","status":"deployed","chart":"oberth-0.17.62"}]`), nil
			case "show":
				return []byte("image:\n  ref: example.invalid/oberth@sha256:" + strings.Repeat("1", 64) + "\n"), nil
			case "template":
				snapshot = args[slices.Index(args, "--values")+1]
				var err error
				validated, err = os.ReadFile(snapshot)
				return nil, err
			case "upgrade":
				applied = true
				if !slices.Contains(args, "--reset-values") || slices.Contains(args, "--reset-then-reuse-values") || args[slices.Index(args, "--values")+1] != snapshot {
					t.Fatalf("wrong apply values: %v", args)
				}
				body, err := os.ReadFile(snapshot)
				if err != nil || !bytes.Equal(body, validated) || string(body) != `{"pushBannerURL":""}` {
					t.Fatalf("apply differed from validation: %s: %v", body, err)
				}
				return nil, stop
			default:
				t.Fatalf("unexpected Helm command: %v", args)
				return nil, nil
			}
		},
	}
	_, err := RunUpgrade(t.Context(), UpgradeConfig{Namespace: "oberth", BinaryVersion: "0.17.63", ChartOverride: "fixture"}, deps)
	if !errors.Is(err, stop) || !applied {
		t.Fatalf("did not reach apply with migration: %v", err)
	}
	if _, err := os.Stat(snapshot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed upgrade leaked private snapshot: %v", err)
	}
}
