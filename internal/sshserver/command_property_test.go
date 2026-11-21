package sshserver

import (
	mathrand "math/rand"
	"path/filepath"
	"strings"
	"testing"
	"testing/quick"
)

func TestArbitraryCommandsNeverPanicOrEscapeRepositoryRoot(t *testing.T) {
	property := func(value string) bool {
		command, err := ParseCommand(value)
		if err != nil {
			return true
		}
		root := filepath.Join(string(filepath.Separator), "repositories")
		relative, err := filepath.Rel(root, filepath.Join(root, command.Repository+".git"))
		return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 10000, Rand: mathrand.New(mathrand.NewSource(1))}); err != nil {
		t.Fatal(err)
	}
}

func FuzzParseCommand(f *testing.F) {
	for _, seed := range []string{"git-upload-pack '", "git-receive-pack '", "git-upload-pack '/origin/team/widget.git'", "git-upload-pack '../../outside'", "", "git-receive-pack \x00"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		command, err := ParseCommand(value)
		if err != nil {
			return
		}
		root := "/repositories"
		relative, err := filepath.Rel(root, filepath.Join(root, command.Repository+".git"))
		if err != nil || relative == ".." || strings.HasPrefix(relative, "../") {
			t.Fatalf("repository escaped root: %q", command.Repository)
		}
	})
}

func TestOnlyGitServicesAreAccepted(t *testing.T) {
	for _, service := range []string{"git-upload-pack", "git-receive-pack", "sh", "git-upload-archive", "other"} {
		_, err := ParseCommand(service + " 'origin/team/widget'")
		if (err == nil) != (service == "git-upload-pack" || service == "git-receive-pack") {
			t.Fatal(service, err)
		}
	}
}
