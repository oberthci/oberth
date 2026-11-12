package installer

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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
