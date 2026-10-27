package integration_test

import (
	"os"
	"strings"
	"testing"
)

func TestServerImageUsesPinnedMinimalContract(t *testing.T) {
	dockerfileRaw, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(dockerfileRaw)
	for _, required := range []string{
		"# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e",
		"FROM --platform=$BUILDPLATFORM golang:1.26.10-alpine3.23@sha256:a07f231c19b03e6106cf442f02a5bcf262147e744d15b7b5e4b6d81d420c70a0 AS build",
		"ENV GOTOOLCHAIN=local GOWORK=off",
		"ARG BUILD_CACHE_NAMESPACE=default",
		"type=cache,id=oberth-server-${BUILD_CACHE_NAMESPACE}-gomod,target=/go/pkg/mod,sharing=locked",
		"type=cache,id=oberth-server-${BUILD_CACHE_NAMESPACE}-gobuild,target=/root/.cache/go-build,sharing=locked",
		// The build line injects main.version from the VERSION build arg
		// (default dev) so release images report the real tag.
		"ARG VERSION=dev",
		`go build -trimpath -buildvcs=false -ldflags="-s -w -X main.version=${VERSION}" -o /out/oberth ./cmd/oberth`,
		"FROM alpine:3.23@sha256:fd791d74b68913cbb027c6546007b3f0d3bc45125f797758156952bc2d6daf40 AS cli-runtime",
		"FROM scratch AS scratch-runtime",
		"FROM cli-runtime AS runtime",
		"COPY third_party/go-git ./third_party/go-git",
		"ca-certificates=20261020-r0",
		"git=2.52.0-r0",
		"libcrypto3=3.5.9-r0",
		"libssl3=3.5.9-r0",
		"pcre2=10.49-r0",
		"libcurl=8.23.0-r0",
		"libexpat=2.9.0-r0",
		"brotli-libs=1.2.1-r0",
		"openssh-client-default=10.2_p1-r0",
		"tzdata=2026e-r0",
		"USER 65534:65534",
		`ENTRYPOINT ["/usr/local/bin/oberth"]`,
		`CMD ["serve"]`,
	} {
		if !strings.Contains(dockerfile, required) {
			t.Fatalf("server Dockerfile lacks %q", required)
		}
	}
	if strings.Count(dockerfile, "type=cache") != 3 ||
		strings.Count(dockerfile, "id=oberth-server-${BUILD_CACHE_NAMESPACE}-gomod") != 2 ||
		strings.Count(dockerfile, "id=oberth-server-${BUILD_CACHE_NAMESPACE}-gobuild") != 1 {
		t.Fatal("every server build cache mount must use the selected isolated namespace")
	}
	for _, forbidden := range []string{
		"# syntax=docker/dockerfile:1.7\n",
		"apk add --no-cache ca-certificates git openssh-client tzdata",
		"GOTOOLCHAIN=auto",
		"USER root",
	} {
		if strings.Contains(dockerfile, forbidden) {
			t.Fatalf("server Dockerfile retains forbidden contract %q", forbidden)
		}
	}
	instructions := serverImageDockerInstructions(dockerfile)
	if instructions[len(instructions)-1] != "FROM cli-runtime AS runtime" {
		t.Fatal("release default must retain the CLI-capable runtime")
	}
	cliStart, scratchStart := -1, -1
	for i, instruction := range instructions {
		if strings.HasPrefix(instruction, "FROM alpine:") {
			cliStart = i
		}
		if instruction == "FROM scratch AS scratch-runtime" {
			scratchStart = i
		}
	}
	if cliStart < 0 || scratchStart <= cliStart {
		t.Fatal("missing CLI runtime before experimental scratch target")
	}
	cli := strings.Join(instructions[cliStart+1:scratchStart], "\n")
	if strings.Count(cli, "apk add --no-cache") != 1 || !strings.Contains(cli, "rm -f /var/log/apk.log") || !strings.Contains(cli, "COPY --from=build /out/oberth /usr/local/bin/oberth") {
		t.Fatal("CLI runtime must install reviewed tools, clean APK log and copy the server")
	}
	scratch := strings.Join(instructions[scratchStart+1:len(instructions)-1], "\n")
	if !strings.Contains(scratch, `CMD ["serve", "--git-backend=go-git"]`) {
		t.Fatal("experimental scratch target must explicitly opt into go-git")
	}

	ignoreRaw, err := os.ReadFile("../../.dockerignore")
	if err != nil {
		t.Fatal(err)
	}
	ignore := "\n" + string(ignoreRaw) + "\n"
	for _, excluded := range []string{".git", ".worktrees", "bin", "dist", "artifacts"} {
		if !strings.Contains(ignore, "\n"+excluded+"\n") {
			t.Fatalf("server build context does not exclude %q", excluded)
		}
	}
}

func serverImageDockerInstructions(dockerfile string) []string {
	var instructions []string
	var current strings.Builder
	for _, raw := range strings.Split(dockerfile, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		continued := strings.HasSuffix(line, `\`)
		line = strings.TrimSpace(strings.TrimSuffix(line, `\`))
		if current.Len() > 0 {
			current.WriteByte(' ')
		}
		current.WriteString(line)
		if !continued {
			instructions = append(instructions, current.String())
			current.Reset()
		}
	}
	if current.Len() > 0 {
		instructions = append(instructions, current.String())
	}
	return instructions
}
