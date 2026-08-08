//go:build ignore

package main

import "oberth"

// ReleaseSecrets is the repository-owned credential request. Oberth admits
// only this subset of the administrator allowlist and mounts no other Secret.
var ReleaseSecrets = []string{"gar-sa-key", "r2-upload-token", "cosign-secret"}

const (
	repositoryToolDirectory  = "/tmp/oberth-tools/bin"
	repositoryToolPath       = repositoryToolDirectory + ":/usr/local/bin:/usr/bin:/bin"
	repositoryGOPATH         = "/tmp/oberth-tools/gopath"
	repositoryLintCache      = "/tmp/oberth-tools/cache/golangci-lint"
	repositoryTrivyCache     = "/tmp/oberth-trivy"
	repositoryDownloadCache  = "/cache/gobuild"
	predecessorLogProbeSpins = 10_000_000
)

// bridgeDeployedPredecessorLogProbe gives deployed server 2edaa393 time to
// attach its known empty pre-start Follow request before the runner emits its
// first step marker. Every real burn and step still runs unchanged. Remove this
// loop is fail-closed behind the interpreter's five-second wall-clock deadline.
// Remove it as soon as a server containing a068d5a3 is live.
func bridgeDeployedPredecessorLogProbe() {
	for spin := 0; spin < predecessorLogProbeSpins; spin++ {
	}
}

func repositorySteps(steps ...oberth.Step) []oberth.Step {
	for index := range steps {
		if commandHasNoPath(steps[index].Command) {
			steps[index].Command = repositoryToolDirectory + "/" + steps[index].Command
		}
		steps[index] = steps[index].
			WithEnv("PATH", repositoryToolPath).
			WithEnv("OBERTH_TOOLS_DIR", "/tmp/oberth-tools").
			WithEnv("HOME", "/tmp").
			WithEnv("GOPATH", repositoryGOPATH).
			WithEnv("GOTOOLCHAIN", "local").
			WithEnv("GOWORK", "off").
			WithEnv("GOENV", "off").
			WithEnv("GOMODCACHE", "/cache/gomod").
			WithEnv("GOCACHE", "/cache/gobuild").
			WithEnv("GIT_TERMINAL_PROMPT", "0").
			WithEnv("KUBEBUILDER_ASSETS", "").
			WithEnv("TRIVY_CACHE_DIR", repositoryTrivyCache).
			WithEnv("GOLANGCI_LINT_CACHE", repositoryLintCache)
	}
	return steps
}

func commandHasNoPath(command string) bool {
	for index := 0; index < len(command); index++ {
		if command[index] == '/' {
			return false
		}
	}
	return true
}

func namedStep(name string, step oberth.Step) oberth.Step {
	step.Name = name
	return step
}

// cachedInstaller keeps executable installation inside the ephemeral Job. The
// mounted cache holds only seven bounded artifact slots; every reuse is copied
// into the Job and checked against repository-owned size and SHA-256 pins.
func cachedInstaller(step oberth.Step) oberth.Step {
	return step.WithEnv("OBERTH_DOWNLOAD_CACHE", repositoryDownloadCache)
}

// releaseBurn uses BurnType wire value 3 because deployed predecessor
// 2edaa393 names that phase Escaped and does not export Release. Remove this
// bridge only after a release built from code exporting Release is live and
// its SSH and MCP acceptance gates pass.
func releaseBurn(name string, dependencies []string, steps ...oberth.Step) oberth.Burn {
	return oberth.Burn{Name: name, Type: oberth.BurnType(3), DependsOn: dependencies, Steps: repositorySteps(steps...)}
}

func releaseActionStep(name, action string) oberth.Step {
	return (oberth.Step{
		Name: name, Command: "oberth-release-support",
		Args: []string{"heartbeat", name, "./.oberth/release.sh", action},
	}).WithEnv("OBERTH_RUNNER_OWNS_RELEASE_GROUP", "1")
}

// Pipeline defines Oberth's own FAB CI. It is interpreted only inside the
// single-container Kubernetes Job.
func Pipeline(ctx *oberth.Context) oberth.Pipeline {
	bridgeDeployedPredecessorLogProbe()
	pipeline := oberth.New().
		Retrograde("setup", repositorySteps(
			cachedInstaller(oberth.Step{Name: "install-go-python", Command: "./.oberth/install-tools.sh", Args: []string{"go", "python"}}),
			cachedInstaller(oberth.Step{Name: "install-zig", Command: "./.oberth/install-tools.sh", Args: []string{"zig"}}),
			cachedInstaller(oberth.Step{Name: "install-golangci", Command: "./.oberth/install-tools.sh", Args: []string{"golangci"}}),
			cachedInstaller(oberth.Step{Name: "install-trivy", Command: "./.oberth/install-tools.sh", Args: []string{"trivy"}}),
			cachedInstaller(oberth.Step{Name: "install-helm", Command: "./.oberth/install-tools.sh", Args: []string{"helm"}}),
		)...).
		Retrograde("lint", repositorySteps(
			ctx.Go.Vet("./..."),
			ctx.Lint.GolangciLint("./..."),
		)...).DependsOn("setup").
		Retrograde("test", repositorySteps(
			ctx.Go.TestRace("./..."),
		)...).DependsOn("lint").
		Retrograde("security", repositorySteps(
			ctx.Scan.TrivyFS("."),
		)...).DependsOn("setup").
		Retrograde("chart", repositorySteps(
			oberth.Step{Name: "helm-contract", Command: "./hack/test-chart.sh"},
			oberth.Step{Name: "node-preflight-contract", Command: "./hack/test-prepare-node.sh"},
		)...).DependsOn("lint").
		Prograde("build-amd64", repositorySteps(
			namedStep("build-oberth-amd64", ctx.Go.Build("./cmd/oberth").WithEnv("GOARCH", "amd64")),
			namedStep("build-oberth-runner-amd64", ctx.Go.Build("./cmd/oberth-runner").WithEnv("GOARCH", "amd64")),
		)...).DependsOn("test", "chart").
		Prograde("build-arm64", repositorySteps(
			namedStep("build-oberth-arm64", ctx.Go.Build("./cmd/oberth").WithEnv("GOARCH", "arm64")),
			namedStep("build-oberth-runner-arm64", ctx.Go.Build("./cmd/oberth-runner").WithEnv("GOARCH", "arm64")),
		)...).DependsOn("test", "chart").
		Build()
	pipeline.Burns = append(pipeline.Burns,
		releaseBurn("release-setup", nil,
			cachedInstaller(oberth.Step{Name: "install-go-python", Command: "./.oberth/install-tools.sh", Args: []string{"go", "python"}}),
			cachedInstaller(oberth.Step{Name: "install-zig", Command: "./.oberth/install-tools.sh", Args: []string{"zig"}}),
			cachedInstaller(oberth.Step{Name: "install-golangci", Command: "./.oberth/install-tools.sh", Args: []string{"golangci"}}),
			cachedInstaller(oberth.Step{Name: "install-trivy", Command: "./.oberth/install-tools.sh", Args: []string{"trivy"}}),
			cachedInstaller(oberth.Step{Name: "install-helm", Command: "./.oberth/install-tools.sh", Args: []string{"helm"}}),
			cachedInstaller(oberth.Step{Name: "install-cosign", Command: "./.oberth/install-tools.sh", Args: []string{"cosign"}}),
			oberth.Step{Name: "build-release-support", Command: "go", Args: []string{"build", "-trimpath", "-o", "/tmp/oberth-tools/bin/oberth-release-support", "./cmd/oberth-release-support"}}.
				WithEnv("CGO_ENABLED", "0"),
			oberth.Step{Name: "build-release-image", Command: "go", Args: []string{"build", "-trimpath", "-o", "/tmp/oberth-tools/bin/oberth-release-image", "./cmd/oberth-release-image"}}.
				WithEnv("CGO_ENABLED", "0"),
			oberth.Step{Name: "validate-tag", Command: "./.oberth/release.sh", Args: []string{"validate"}}.
				WithEnv("OBERTH_RELEASE_TAG", ctx.Tag).
				WithEnv("OBERTH_RELEASE_SHA", ctx.SHA),
		),
		releaseBurn("release-lint", []string{"release-setup"},
			ctx.Go.Vet("./..."),
			ctx.Lint.GolangciLint("./..."),
		),
		releaseBurn("release-scan", []string{"release-setup"}, ctx.Scan.TrivyFS(".")),
		releaseBurn("release-test", []string{"release-lint", "release-scan"}, ctx.Go.TestRace("./...")),
		releaseBurn("release-chart-test", []string{"release-setup"},
			oberth.Step{Name: "helm-contract", Command: "./hack/test-chart.sh"},
			oberth.Step{Name: "node-preflight-contract", Command: "./hack/test-prepare-node.sh"},
		),
		releaseBurn("release-build", []string{"release-test", "release-chart-test"},
			releaseActionStep("build-reproducible-artifacts", "build").
				WithEnv("OBERTH_RELEASE_TAG", ctx.Tag).
				WithEnv("OBERTH_RELEASE_SHA", ctx.SHA).
				WithTimeout(oberth.Hour),
		),
		releaseBurn("release-publish-r2", []string{"release-build"},
			releaseActionStep("publish-signed-binaries", "publish-r2").
				WithEnv("OBERTH_RELEASE_TAG", ctx.Tag).
				WithEnv("OBERTH_RELEASE_SHA", ctx.SHA).
				WithTimeout(oberth.Hour),
		),
		releaseBurn("release-publish-images", []string{"release-build"},
			releaseActionStep("publish-scan-sign-images", "publish-images").
				WithEnv("OBERTH_RELEASE_TAG", ctx.Tag).
				WithEnv("OBERTH_RELEASE_SHA", ctx.SHA).
				WithTimeout(oberth.Hour),
		),
		releaseBurn("release-package-chart", []string{"release-publish-images"},
			releaseActionStep("package-digest-pinned-chart", "package-chart").
				WithEnv("OBERTH_RELEASE_TAG", ctx.Tag).
				WithEnv("OBERTH_RELEASE_SHA", ctx.SHA),
		),
		releaseBurn("release-publish-chart", []string{"release-publish-r2", "release-package-chart"},
			releaseActionStep("publish-signed-chart", "publish-chart").
				WithEnv("OBERTH_RELEASE_TAG", ctx.Tag).
				WithEnv("OBERTH_RELEASE_SHA", ctx.SHA).
				WithTimeout(oberth.Hour),
		),
		releaseBurn("release-verify", []string{"release-publish-chart"},
			releaseActionStep("verify-public-and-gar-release", "verify").
				WithEnv("OBERTH_RELEASE_TAG", ctx.Tag).
				WithEnv("OBERTH_RELEASE_SHA", ctx.SHA).
				WithTimeout(oberth.Hour),
		),
		releaseBurn("release-finalize", []string{"release-verify"},
			releaseActionStep("advance-stable-aliases", "finalize").
				WithEnv("OBERTH_RELEASE_TAG", ctx.Tag).
				WithEnv("OBERTH_RELEASE_SHA", ctx.SHA).
				WithTimeout(oberth.Hour),
		),
	)
	return pipeline
}
