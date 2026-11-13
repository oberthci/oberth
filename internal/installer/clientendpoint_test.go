package installer

import (
	"context"
	"io"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAdvertisedEndpointChangeReconcilesInstalledRelease(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"host", Config{ClientHost: "localhost"}, `clientEndpoint.host="localhost"`},
		{"ssh", Config{ClientSSHPort: "32022"}, `clientEndpoint.sshPort="32022"`},
		{"https", Config{ClientHTTPSPort: "32443"}, `clientEndpoint.httpsPort="32443"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			deps := Deps{Output: io.Discard, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
				if len(args) > 1 && args[0] == "get" && args[1] == "values" {
					return []byte(`{}`), nil
				}
				if args[0] == "list" {
					return []byte(`[{"name":"oberth","chart":"oberth-0.17.62","status":"deployed"}]`), nil
				}
				if args[0] == "show" {
					return []byte("image:\n  ref: example.invalid/oberth@sha256:" + strings.Repeat("1", 64) + "\n"), nil
				}
				if args[0] == "upgrade" {
					writes++
					if !strings.Contains(strings.Join(args, " "), tc.want) {
						t.Fatal("reconcile lost advertised endpoint")
					}
				}
				return nil, nil
			}}
			withSignedRelease(t, &deps, "0.17.62", "example.invalid/oberth@sha256:"+strings.Repeat("1", 64))
			tc.cfg.ChartVersion = "v0.17.62"
			result, err := InstallOberth(t.Context(), tc.cfg, deps, OpenBaoResult{}, RekorResult{})
			if err != nil || writes != 1 || !result.Upgraded {
				t.Fatalf("endpoint change skipped on rerun: err=%v writes=%d upgraded=%v", err, writes, result.Upgraded)
			}
		})
	}
}

func TestAdvertisedEndpointHelmValuesAndValidation(t *testing.T) {
	args := strings.Join(OberthHelmArgs(Config{ClientHost: "localhost", ClientSSHPort: "32022", ClientHTTPSPort: "32443"}, OpenBaoResult{}, RekorResult{}), " ")
	for _, value := range []string{`--set-json clientEndpoint.host="localhost"`, `--set-json clientEndpoint.sshPort="32022"`, `--set-json clientEndpoint.httpsPort="32443"`} {
		if !strings.Contains(args, value) {
			t.Fatalf("install does not persist %s", value)
		}
	}
	// Unspecified overrides must survive Helm --reuse-values on a rerun.
	if args := strings.Join(OberthHelmArgs(Config{}, OpenBaoResult{}, RekorResult{}), " "); strings.Contains(args, "clientEndpoint.") {
		t.Fatal("install without flags clears the saved endpoint")
	}
	for _, annotation := range []map[string]string{
		{clientHostAnnotation: "https://invalid/path"},
		{clientSSHPortAnnotation: "65536"},
		{clientHTTPSPortAnnotation: "032443"},
	} {
		if _, err := withStoredClientEndpoint(&corev1.Service{ObjectMeta: metav1.ObjectMeta{Annotations: annotation}}, Deps{}); err == nil {
			t.Fatal("invalid saved endpoint accepted")
		}
	}
}
