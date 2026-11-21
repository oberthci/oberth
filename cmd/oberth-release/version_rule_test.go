package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReleaseVersionRuleMatchesPublicationScript(t *testing.T) {
	for _, version := range []string{"v0.0.0", "v1.2.3", "v12.34.56", "v01.2.3", "v1.2.3junk", "v1.2.3/x", "v1.2.3.4", "v1.2.3-rc.1", "v1.2.3+build", "v1.2", "1.2.3"} {
		t.Run(version, func(t *testing.T) {
			env := releaseEnvironment(t)
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\nprintf '%040d\\n' 0\n"), 0700); err != nil {
				t.Fatal(err)
			}
			env["PATH"] = dir + string(os.PathListSeparator) + os.Getenv("PATH")
			env["VERSION"], env["RELEASE_BUILD_STATUS"] = version, "passed"
			env["RELEASE_TESTED_COMMIT"], env["RELEASE_DEFAULT_COMMIT"] = "0000000000000000000000000000000000000000", "0000000000000000000000000000000000000000"
			valid := version == "v0.0.0" || version == "v1.2.3" || version == "v12.34.56"
			buildOutput, buildErr := buildScript(t, releaseBuildTree(t), map[string]string{"VERSION": version})
			if (buildErr == nil) != valid {
				t.Fatal("build and publication version rules differ", buildErr, string(buildOutput))
			}
			if err := packageRelease(t.Context(), options{version: version}); errors.Is(err, inputs) == valid {
				t.Fatal("Go packaging uses another version rule", err)
			}
			output, err := releaseScript(t, "check", env)
			if (err == nil) != valid {
				t.Fatal("script and admission version rules differ", err, string(output))
			}
		})
	}
}
