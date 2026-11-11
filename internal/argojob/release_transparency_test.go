package argojob

import (
	"os"
	"strings"
	"testing"
)

// Release signatures must be recorded in the public transparency log and
// every verification must require that entry. Legacy (<= v0.17.95) bundles
// are re-verified only by their own tagged release.sh.
func TestReleaseSignaturesRequireTransparencyLog(t *testing.T) {
	for _, path := range []string{"../../.oberth/release.sh", "../../.oberth/release.yaml"} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"--insecure-ignore-tlog", "--tlog-upload=false", "--use-signing-config=false", "--signing-config", "--trusted-root", "--private-infrastructure"} {
			if strings.Contains(string(content), forbidden) {
				t.Errorf("%s weakens release transparency with %s", path, forbidden)
			}
		}
	}
	script, err := os.ReadFile("../../.oberth/release.sh")
	if err != nil {
		t.Fatal(err)
	}
	helper := string(script)
	helperStart := strings.Index(helper, "checksum_signature() (")
	if helperStart < 0 {
		t.Fatal("checksum_signature is missing")
	}
	helper = helper[helperStart:]
	helper = helper[:strings.Index(helper, "\n)\n")]
	if !strings.Contains(helper, `"$cosign_tool" verify-blob --key "$release_public_key"`) {
		t.Fatal("checksum signature helper must authenticate the Sigstore bundle")
	}
	runtime := string(script)
	start := strings.Index(runtime, "verify_runtime_payload() {")
	if start < 0 {
		t.Fatal("verify_runtime_payload is missing")
	}
	runtime = runtime[start:]
	runtime = runtime[:strings.Index(runtime, "\n}\n")]
	if !strings.Contains(runtime, `TUF_ROOT=$runtime_tuf_root checksum_signature "$release_dir"`) {
		t.Error("runtime verification after executing the candidate must use a fresh TUF cache")
	}
}
