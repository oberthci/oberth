package integration_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/staticgit"
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
		"FROM scratch AS runtime",
		"ca-certificates=20261020-r0",
		"26c56c296b38c0695b26fa95f475f1d01704d2d38e73465ca30b0b2f5dc789d3",
		"COPY --from=build --chmod=0755 /out/ /usr/local/bin/",
		"COPY --from=git-build --chmod=0755 /out/git /usr/bin/git",
		"ln -s oberth /out/oberth-git-ssh",
		"USER 65534:65534",
		`ENTRYPOINT ["/usr/local/bin/oberth"]`,
		`CMD ["serve"]`,
	} {
		if !strings.Contains(dockerfile, required) {
			t.Fatalf("server Dockerfile lacks %q", required)
		}
	}
	recipe, err := os.ReadFile("../../hack/build-static-git.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"NO_CURL=YesPlease NO_OPENSSL=YesPlease NO_EXPAT=YesPlease", "NO_PERL=YesPlease NO_PYTHON=YesPlease NO_TCLTK=YesPlease NO_GETTEXT=YesPlease", "LDFLAGS=-static", "--fuzz=0", "$GIT_SOURCE_SHA256", "$GIT_PATCH_SHA256"} {
		if !strings.Contains(string(recipe), required) {
			t.Fatalf("canonical static Git recipe lacks %q", required)
		}
	}
	for key, value := range staticgit.Labels() {
		if !strings.Contains(dockerfile, fmt.Sprintf("%s=\"%s\"", key, value)) {
			t.Fatalf("runtime component label %s drifted", key)
		}
	}
	if !strings.Contains(dockerfile, "/bin/sh /tmp/install-static-git-packages.sh /pins/static-git.env") || !strings.Contains(dockerfile, "/bin/sh /tmp/build-static-git.sh /pins/static-git.env") {
		t.Fatal("Dockerfile bypassed the pinned canonical Git build")
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
	start := -1
	for i, instruction := range instructions {
		if strings.HasPrefix(instruction, "FROM ") {
			start = i
		}
	}
	if start < 0 || instructions[start] != "FROM scratch AS runtime" {
		t.Fatal("runtime must be scratch")
	}
	for _, instruction := range instructions[start+1:] {
		if strings.HasPrefix(instruction, "RUN ") || strings.Contains(instruction, "/bin/sh") || strings.Contains(instruction, "/usr/bin/ssh") {
			t.Fatalf("runtime retains shell or SSH tooling: %s", instruction)
		}
		if strings.HasPrefix(instruction, "COPY ") && instruction != "COPY --from=build --chmod=0755 /out/ /usr/local/bin/" && instruction != "COPY --from=git-build --chmod=0755 /out/git /usr/bin/git" && instruction != "COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt" {
			t.Fatalf("unexpected runtime payload: %s", instruction)
		}
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
