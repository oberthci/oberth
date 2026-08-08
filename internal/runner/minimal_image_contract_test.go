package runner

import (
	"os"
	"strings"
	"testing"
)

func TestRunnerImageContainsOnlyBootstrapRuntime(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile("../../Dockerfile.runner")
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(body)
	const frontend = `# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e`
	if !strings.HasPrefix(dockerfile, frontend+"\n") {
		t.Fatal("Dockerfile.runner lacks its immutable Dockerfile frontend")
	}

	instructions := dockerInstructions(dockerfile)
	normalized := strings.Join(instructions, " ")
	for name, required := range map[string]string{
		"source epoch":       `ARG SOURCE_DATE_EPOCH`,
		"runner build":       `go build -trimpath -buildvcs=false -ldflags='-s -w -buildid=' -o /out/oberth-runner ./cmd/oberth-runner`,
		"minimal base":       `FROM alpine:3.23@sha256:fd791d74b68913cbb027c6546007b3f0d3bc45125f797758156952bc2d6daf40 AS runner`,
		"bootstrap packages": `apk add --no-cache bash=5.3.3-r1 ca-certificates=20260611-r0 curl=8.20.0-r0 git=2.52.0-r0`,
		"discard apk log":    `rm -f /var/log/apk.log`,
		"runner binary":      `COPY --from=build /out/oberth-runner /usr/local/bin/oberth-runner`,
		"writable tool path": `PATH=/tmp/oberth-tools/bin:/usr/local/bin:/usr/bin:/bin`,
		"non-root user":      `USER 65534:65534`,
		"entrypoint":         `ENTRYPOINT ["/usr/local/bin/oberth-runner"]`,
	} {
		if !strings.Contains(normalized, required) {
			t.Fatalf("Dockerfile.runner lacks %s %q", name, required)
		}
	}

	for _, forbidden := range []string{
		" AS tools", " AS envtest", " AS trivydb", " AS tools-export",
		"/seed", "build-base", "coreutils", "jq=", "make=", "openssh-client",
		"python", "xz=", "zip=", "unzip=", "golangci-lint", "trivy", "helm", "cosign",
		"kube-apiserver", "kubectl", "etcd", "KUBEBUILDER_ASSETS",
		"TRIVY_CACHE_DIR", "GOLANGCI_LINT_CACHE", "go install",
	} {
		if strings.Contains(strings.ToLower(dockerfile), strings.ToLower(forbidden)) {
			t.Fatalf("Dockerfile.runner bakes forbidden dependency %q", forbidden)
		}
	}

	finalFrom := -1
	for index, instruction := range instructions {
		if strings.HasPrefix(instruction, "FROM ") {
			finalFrom = index
		}
	}
	if finalFrom < 0 || instructions[finalFrom] != "FROM alpine:3.23@sha256:fd791d74b68913cbb027c6546007b3f0d3bc45125f797758156952bc2d6daf40 AS runner" {
		t.Fatalf("final runner stage = %q", instructions[finalFrom])
	}
	finalStage := strings.Join(instructions[finalFrom:], " ")
	if strings.Count(finalStage, "apk add") != 1 {
		t.Fatalf("final runner stage contains %d package installs, want 1", strings.Count(finalStage, "apk add"))
	}
}

func dockerInstructions(dockerfile string) []string {
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
