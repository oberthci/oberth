package argojob

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKVMRuntimeSBOMUsesReleaseOutputWithReadOnlySource(t *testing.T) {
	for _, mode := range []string{"success", "apko-failure", "empty-tar"} {
		t.Run(mode, func(t *testing.T) {
			f := newReleaseRuntimeFixture(t)
			source, err := os.ReadFile(f.script)
			if err != nil {
				t.Fatal(err)
			}
			index := strings.LastIndex(string(source), "\ncase \"$action\" in\n")
			if index < 0 {
				t.Fatal("missing release dispatch")
			}
			// Execute the production build function; replace only top-level dispatch.
			f.script = filepath.Join(f.root, "runtime-sbom.sh")
			f.absolute(f.script, string(source[:index])+"\nbuild_kvm_runtime\n", 0700)
			f.tool("apko", `test "$1" = build
test "$2" = --sbom-path
test "$3" = "$FIXTURE_CANDIDATE"
test "$4" = --lockfile
test "$5" = packaging/kvm-harness/runtime.apko.lock.json
test "$6" = packaging/kvm-harness/runtime.apko.yaml
test "$7" = runtime:latest
test "$8" = "$FIXTURE_CANDIDATE/kvm-runtime.tar"
test "$#" = 8
if [ "$FIXTURE_APKO_MODE" = apko-failure ]; then exit 42; fi
printf 'inert SPDX fixture\n' >"$3/sbom-x86_64.spdx.json"
if [ "$FIXTURE_APKO_MODE" = empty-tar ]; then : >"$8"; else printf 'inert OCI fixture\n' >"$8"; fi`)
			f.env = []string{"FIXTURE_APKO_MODE=" + mode}
			if err := os.Chmod(f.root, 0500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(f.root, 0700) })
			output, err := f.run("publish-kvm-harness", false)
			if mode == "success" {
				if err != nil {
					t.Fatalf("runtime build: %v\n%s", err, output)
				}
				if _, err := os.Stat(filepath.Join(f.candidate, "sbom-x86_64.spdx.json")); err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("failed or empty apko output accepted")
			}
			if _, err := os.Stat(filepath.Join(f.root, "sbom-x86_64.spdx.json")); !os.IsNotExist(err) {
				t.Fatal("SBOM escaped into source working directory")
			}
		})
	}
}
