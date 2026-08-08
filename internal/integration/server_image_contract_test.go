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
		"FROM golang:1.26.5-alpine3.23@sha256:622e56dbc11a8cfe87cafa2331e9a201877271cbff918af53d3be315f3da88cc AS build",
		"ENV GOTOOLCHAIN=local GOWORK=off",
		"ARG BUILD_CACHE_NAMESPACE=default",
		"type=cache,id=oberth-server-${BUILD_CACHE_NAMESPACE}-gomod,target=/go/pkg/mod,sharing=locked",
		"type=cache,id=oberth-server-${BUILD_CACHE_NAMESPACE}-gobuild,target=/root/.cache/go-build,sharing=locked",
		"go build -trimpath -buildvcs=false -ldflags='-s -w' -o /out/oberth ./cmd/oberth",
		"FROM alpine:3.23@sha256:fd791d74b68913cbb027c6546007b3f0d3bc45125f797758156952bc2d6daf40",
		"ca-certificates=20260611-r0",
		"git=2.52.0-r0",
		"openssh-client-default=10.2_p1-r0",
		"tzdata=2026c-r0",
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
	finalStageStart := -1
	for index, instruction := range instructions {
		if instruction == "FROM alpine:3.23@sha256:fd791d74b68913cbb027c6546007b3f0d3bc45125f797758156952bc2d6daf40" {
			finalStageStart = index
		}
	}
	if finalStageStart < 0 {
		t.Fatal("server runtime stage is missing")
	}
	packageInstall := ""
	apkCommands := 0
	for _, instruction := range instructions[finalStageStart+1:] {
		if strings.HasPrefix(instruction, "FROM ") {
			t.Fatal("server runtime stage must be the final stage")
		}
		apkCommands += serverImageAPKCommandCount(instruction)
		if strings.HasPrefix(instruction, "RUN ") && strings.Contains(instruction, "apk add --no-cache") {
			packageInstall = instruction
		}
	}
	install := strings.Index(packageInstall, "apk add --no-cache")
	cleanup := strings.Index(packageInstall, "rm -f /var/log/apk.log")
	if apkCommands != 1 || install < 0 || cleanup <= install {
		t.Fatal("server runtime stage must remove the APK log after its sole package install")
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

func serverImageAPKCommandCount(instruction string) int {
	count := 0
	for _, field := range strings.Fields(instruction) {
		command := strings.Trim(field, `"'();&|`)
		if command == "apk" || command == "/sbin/apk" {
			count++
		}
	}
	return count
}
