package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestReleaseLayoutWorksWithAgentFetcher(t *testing.T) {
	o := setup(t)
	packageFixture(t, o)
	directory, err := filepath.Abs(o.dir)
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	source := `import pathlib, sys
sys.path.insert(0, str(pathlib.Path(sys.argv[1]) / "agent"))
from fetch_support import FetchCase, serve
case = FetchCase("runTest")
case.setUp()
try:
    release = pathlib.Path(sys.argv[2])
    binary = (release / case.artifact).read_bytes()
    case.release(version="v1.2.3", binary=binary,
                 manifest=(release / "SHA256SUMS").read_bytes(),
                 record=(release / "release.json").read_bytes())
    with serve(case.files, case.requests) as origin:
        result = case.fetch(origin, "v1.2.3")
    if result.returncode:
        raise AssertionError(result.stderr.decode())
    assert (case.home / ".local/bin/oberth").read_bytes() == binary
    assert "/v1.2.3/release.json" in case.requests
    assert "/v1.2.3/SHA256SUMS.sigstore.json" in case.requests
finally:
    case.doCleanups()
`
	command := exec.CommandContext(ctx, "python3", "-c", source, root, directory)
	command.Env = os.Environ()
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("agent contract: %v %s", err, output)
	}
}
