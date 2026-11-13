package argojob

import (
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestBuildTypesDocumentRefusalsButNotOperatorConfiguration(t *testing.T) {
	for _, kind := range []string{"decode", "admit", "document-isolation", "config", "vault-address", "ci-role", "mirror", "identity"} {
		t.Run(kind, func(t *testing.T) {
			config := testConfig()
			request := testRequest(pipeline.TriggerCI, greedyDocument)
			doc := true
			switch kind {
			case "decode":
				request.Source = []byte("apiVersion: argoproj.io/v1alpha1\nkind: Secret\n")
			case "admit":
				request.Source = []byte(strings.Replace(greedyDocument, "spec:", "spec:\n  hostNetwork: true", 1))
			case "document-isolation":
				request.Source = []byte(strings.Replace(greedyDocument, "spec:", "spec:\n  templateDefaults:\n    metadata:\n      annotations:\n        oberth.ci/workspace-mounts: none", 1))
			case "config":
				config.Namespace = "INVALID namespace"
				doc = false
			case "mirror":
				request.Source = []byte(strings.Replace(greedyDocument, "    example.com/tier: L", "    example.com/tier: L\n    oberth.ci/goproxy: mirror-only", 1))
				config.GoProxyURL = ""
				doc = false
			default:
				request.Source = []byte(strings.Replace(greedyDocument, "    example.com/tier: L", "    example.com/tier: L\n    oberth.ci/secret-paths: oberth/upstream/acme/oberth/test-secret", 1))
				request.ApprovedSecrets = map[string]bool{"oberth/upstream/acme/oberth/test-secret": true}
				request.StepGrants = map[string]map[string]bool{"oberth/upstream/acme/oberth/test-secret": {"*": true}}
				request.Source = []byte(strings.Replace(string(request.Source), "command: [/bin/true]", "command: [oberth, secretstore, exec, --path=oberth/upstream/acme/oberth/test-secret, --, /bin/true]", 1))
				doc = false
				switch kind {
				case "vault-address":
					config.VaultAddress = ""
					config.VaultCredentialedRole = ""
				case "ci-role":
					config.VaultCISecretsRole = ""
				case "identity":
					config.CISecretsServiceAccount = "bad/identity"
				}
			}
			_, err := Build(config, request)
			if err == nil {
				t.Fatal("fixture did not fail", kind)
			}
			var refusal *argoworkflow.AdmissionError
			if got := errors.As(err, &refusal); got != doc {
				t.Fatalf("%s classified document=%v, want %v: %v", kind, got, doc, err)
			}
		})
	}
}
