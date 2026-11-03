package argojob

import (
	"os"
	"strings"
	"testing"
)

func TestReleaseOnlyPublishesProductArtifacts(t *testing.T) {
	workflow, err := os.ReadFile("../../.oberth/release.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"release-build", "release-publish-images", "release-publish-chart", "release-publish-public", "release-finalize", "release-runtime"} {
		if !strings.Contains(string(workflow), "name: "+phase+"\n") {
			t.Errorf("product release phase missing: %s", phase)
		}
	}
	for _, forbidden := range []string{"publish-website", "website-readback", "verify-website-inputs", "install-website-tools", "stage-website-packages"} {
		if strings.Contains(string(workflow), forbidden) {
			t.Errorf("product release still deploys or prepares the separate site: %s", forbidden)
		}
	}
	script, err := os.ReadFile("../../.oberth/release.sh")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(script), "publish_website()") || strings.Contains(string(script), "wrangler deploy") {
		t.Fatal("product release script still contains site deployment authority")
	}
}
