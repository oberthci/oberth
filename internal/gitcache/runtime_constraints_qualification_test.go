//go:build staticgit_qualification && linux

package gitcache

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestStaticGitQualificationRuntimeConstraints(t *testing.T) {
	if os.Getuid() != 65534 || os.Geteuid() != 65534 || os.Getgid() != 65534 || os.Getegid() != 65534 {
		t.Fatal("qualification requires UID/GID 65534")
	}
	for _, path := range []string{"/", "/qualification", "/usr/bin/git", "/usr/local/bin"} {
		var filesystem unix.Statfs_t
		if err := unix.Statfs(path, &filesystem); err != nil || filesystem.Flags&unix.ST_RDONLY == 0 {
			t.Fatalf("qualification input %s is not read-only: %v", path, err)
		}
	}
	if _, err := exec.LookPath("sh"); err == nil {
		t.Fatal("a shell is reachable through qualification PATH")
	}
	if err := exec.Command("/bin/sh", "-c", "exit 0").Run(); err == nil || (!errors.Is(err, os.ErrPermission) && !errors.Is(err, os.ErrNotExist)) {
		t.Fatalf("ambient shell was not denied: %v", err)
	}
	if os.Getenv("GIT_SSH") != "/usr/local/bin/oberth-git-ssh" || os.Getenv("GIT_SSH_VARIANT") != "simple" || os.Getenv("GIT_SSH_COMMAND") != "" {
		t.Fatal("qualification is not bound to native GIT_SSH")
	}
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"CapEff:", "CapPrm:"} {
		found := false
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, field) {
				values := strings.Fields(line)
				found = len(values) == 2 && strings.Trim(values[1], "0") == ""
			}
		}
		if !found {
			t.Fatalf("qualification retained capabilities: %s", field)
		}
	}
}
