package argojob

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Synthetic key material only. The fake IAM exchange binds each source credential to its own role; the
// releaseauth tests independently exercise the real request and denial paths.
func releaseCredentialJSON(account string) string {
	return fmt.Sprintf(`{"type":"service_account","project_id":"skipopsmain","client_email":%q,"private_key":"PRIVATE_CREDENTIAL_SENTINEL"}`, account+"@skipopsmain.iam.gserviceaccount.com")
}

func TestReleaseCredentialsBindActionToDelegatedRole(t *testing.T) {
	for _, tc := range []struct{ action, path, account string }{
		{"publish-images", "gar-image-key", "source-image-delegator"},
		{"publish-chart", "gar-chart-key", "source-chart-delegator"},
		{"verify", "gar-reader-key", "source-read-delegator"},
	} {
		t.Run(tc.action, func(t *testing.T) {
			for _, scenario := range []string{"valid", "valid-ambient-poison", "legacy-iac", "image-writer", "chart-writer", "reader", "missing", "empty", "malformed", "array", "wrong-type", "wrong-project", "missing-email", "email-object", "duplicate-email", "duplicate-type", "nonfinite", "oversized", "symlink", "ambient-override"} {
				t.Run(scenario, func(t *testing.T) {
					f := newReleaseRuntimeFixture(t)
					key := filepath.Join("secrets", tc.path, "GAR_SA_KEY")
					body := releaseCredentialJSON(tc.account)
					valid := scenario == "valid"
					switch scenario {
					case "valid-ambient-poison":
						valid = true
						f.env = []string{"GAR_EXPECTED_CLIENT_EMAIL=terraform@skipopsmain.iam.gserviceaccount.com", "OBERTH_GAR_CLIENT_EMAIL=terraform@skipopsmain.iam.gserviceaccount.com", "PYTHONPATH=" + f.root, "PYTHONSTARTUP=" + filepath.Join(f.root, "sitecustomize.py")}
						f.write("sitecustomize.py", "raise RuntimeError('PRIVATE_CREDENTIAL_SENTINEL')")
					case "legacy-iac":
						body = releaseCredentialJSON("terraform")
					case "image-writer", "chart-writer", "reader":
						account := map[string]string{"image-writer": "source-image-delegator", "chart-writer": "source-chart-delegator", "reader": "source-read-delegator"}[scenario]
						body, valid = releaseCredentialJSON(account), account == tc.account
					case "missing", "symlink":
						if err := os.Remove(filepath.Join(f.root, key)); err != nil {
							t.Fatal(err)
						}
					case "empty":
						body = ""
					case "malformed":
						body = "PRIVATE_CREDENTIAL_SENTINEL {"
					case "array":
						body = "[" + body + "]"
					case "wrong-type":
						body = strings.Replace(body, "service_account", "authorized_user", 1)
					case "wrong-project":
						body = strings.Replace(body, `"project_id":"skipopsmain"`, `"project_id":"foreign"`, 1)
					case "missing-email":
						body = strings.Replace(body, "client_email", "not_client_email", 1)
					case "email-object":
						body = strings.Replace(body, fmt.Sprintf("%q", tc.account+"@skipopsmain.iam.gserviceaccount.com"), `{}`, 1)
					case "duplicate-email":
						body = `{"client_email":"terraform@skipopsmain.iam.gserviceaccount.com",` + body[1:]
					case "duplicate-type":
						body = `{"type":"authorized_user",` + body[1:]
					case "nonfinite":
						body = `{"unexpected":NaN,` + body[1:]
					case "oversized":
						body = strings.Replace(body, "PRIVATE_CREDENTIAL_SENTINEL", strings.Repeat("X", 65537), 1)
					case "ambient-override":
						body = releaseCredentialJSON("terraform")
						f.env = []string{"GAR_EXPECTED_CLIENT_EMAIL=terraform@skipopsmain.iam.gserviceaccount.com", "OBERTH_GAR_CLIENT_EMAIL=terraform@skipopsmain.iam.gserviceaccount.com", "PYTHONPATH=" + f.root, "PYTHONSTARTUP=" + filepath.Join(f.root, "sitecustomize.py")}
						f.write("sitecustomize.py", "raise RuntimeError('PRIVATE_CREDENTIAL_SENTINEL')")
					}
					if scenario == "symlink" {
						f.write("other-key", body)
						if err := os.Symlink(filepath.Join(f.root, "other-key"), filepath.Join(f.root, key)); err != nil {
							t.Fatal(err)
						}
					} else if scenario != "missing" {
						f.write(key, body)
					}
					// All alternative credentials remain present, including the old
					// broad writer. A missing/rejected key must never fall back.
					f.write("secrets/gar-sa-key/GAR_SA_KEY", releaseCredentialJSON("terraform"))
					f.tool("oberth-release-image", `printf 'image:%s\n' "$1" >>"$FIXTURE_TRACE"
test "$6" = "$FIXTURE_ROOT/secrets/.runtime/.gar-token"
test "$(cat "$6")" = fixture-restricted-access-token`)
					helm := strings.Replace(immutableChartFixture, "registry) cat >/dev/null;;", `registry) test "$(cat)" = fixture-restricted-access-token;;`, 1)
					f.tool("helm", strings.Replace(helm, "*) exit 90;;", `template) cat "$FIXTURE_CANDIDATE/server-image.txt";;
*) exit 90;;`, 1))
					out, err := f.run(tc.action, true)
					if strings.Contains(out+f.log(), "PRIVATE_CREDENTIAL_SENTINEL") {
						t.Fatal("credential content escaped to diagnostics")
					}
					if valid {
						if err != nil {
							t.Fatalf("valid identity rejected: %v\n%s\n%s", err, out, f.log())
						}
					} else {
						if err == nil || !strings.Contains(out, "GAR role delegation failed") {
							t.Fatalf("invalid identity was not rejected by guard: %v\n%s", err, out)
						}
						if log := f.log(); log != "" {
							t.Fatalf("invalid identity reached external tools:\n%s", log)
						}
					}
				})
			}
		})
	}
}
