package argojob

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestReleaseApkoExtractionPreservesInvokerOwnershipAndPin(t *testing.T) {
	workflow, err := argoworkflow.Decode(loadPipeline(t, "release.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var program string
	for _, template := range workflow.Spec.Templates {
		if template.Name == "verify-apko" && template.Container != nil {
			program = template.Container.Args[len(template.Container.Args)-1]
		}
	}
	if !strings.Contains(program, "tar --no-same-owner -xzf apko.tar.gz") {
		t.Fatal("release apko extraction must preserve local ownership without CAP_CHOWN")
	}
	match := regexp.MustCompile(`mv (apko_[^/ ]+)/apko`).FindStringSubmatch(program)
	if len(match) != 2 {
		t.Fatal("apko archive layout is not explicit")
	}
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprintf("corrupt_%t", corrupt), func(t *testing.T) {
			dir := t.TempDir()
			payload := []byte("inert apko executable fixture\n")
			expected := sha256.Sum256(payload)
			if corrupt {
				payload = append(payload, []byte("tampered")...)
			}
			file, err := os.Create(filepath.Join(dir, "apko.tar.gz"))
			if err != nil {
				t.Fatal(err)
			}
			compressed := gzip.NewWriter(file)
			archive := tar.NewWriter(compressed)
			for _, entry := range []struct {
				name string
				data []byte
			}{{"LICENSE", []byte("fixture license")}, {"apko", payload}} {
				if err := archive.WriteHeader(&tar.Header{Name: match[1] + "/" + entry.name, Mode: 0o755, Size: int64(len(entry.data)), Uid: 1001, Gid: 1001}); err != nil {
					t.Fatal(err)
				}
				if _, err := archive.Write(entry.data); err != nil {
					t.Fatal(err)
				}
			}
			for _, close := range []func() error{archive.Close, compressed.Close, file.Close} {
				if err := close(); err != nil {
					t.Fatal(err)
				}
			}
			pin := filepath.Join(dir, "pin.sha256")
			if err := os.WriteFile(pin, fmt.Appendf(nil, "%x  %s\n", expected, filepath.Join(dir, "apko")), 0o600); err != nil {
				t.Fatal(err)
			}
			script := strings.ReplaceAll(program, "/tmp/oberth-tools/bin", dir)
			script = strings.ReplaceAll(script, "/work/src/.oberth/pins/apko.sha256", pin)
			command := exec.CommandContext(t.Context(), "/bin/sh", "-ec", script)
			command.Env = []string{"PATH=/usr/bin:/bin"}
			output, err := command.CombinedOutput()
			if corrupt {
				if err == nil || !strings.Contains(string(output), "FAILED") {
					t.Fatalf("corrupt executable accepted: %v %s", err, output)
				}
				return
			}
			if err != nil {
				t.Fatalf("extract and verify: %v %s", err, output)
			}
			info, err := os.Stat(filepath.Join(dir, "apko"))
			if err != nil {
				t.Fatal(err)
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Gid != uint32(os.Getegid()) {
				t.Fatal("archive ownership replaced local ownership")
			}
		})
	}
}
