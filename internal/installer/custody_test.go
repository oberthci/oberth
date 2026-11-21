package installer

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCustodyFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	c, err := OpenConfigDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err = c.save("private", []byte("private material")); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, "private"))
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal(st, err)
	}
	if err = c.save("private", []byte("different")); err == nil {
		t.Fatal("exclusive file replaced")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "private"))
	if err != nil || string(raw) != "private material" {
		t.Fatal("private file changed", err)
	}
	assertCustodyLinks(t, c, dir)
	assertCustodyProfile(t, c)
}
func TestCredentialDirectoryRefusals(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if c, err := OpenConfigDir(link); err != nil {
		t.Fatal("ordinary profile link refused", err)
	} else {
		_ = c.Close()
	}
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if c, err := OpenConfigDir(target); err == nil {
		_ = c.Close()
		t.Fatal("checkout accepted")
	}
}
func TestInstallMessages(t *testing.T) {
	for _, phase := range []Phase{Checking, ReleaseChecked, Creating, Saving, Installing, Waiting, Endpoint, Installed} {
		if len(phase) > 40 || strings.Contains(string(phase), "…") || strings.HasSuffix(string(phase), "...") {
			t.Fatal(phase)
		}
	}
	for name, p := range problems() {
		if len(p.Reason) > 80 || len(p.Remedy) > 120 || strings.Contains(p.Error(), "…") || strings.Contains(p.Error(), "...") {
			t.Fatal(name, p)
		}
	}
	t.Run("upgrade review", checkUpgradeReviewMessages)
}

func checkUpgradeReviewMessages(t *testing.T) {
	for _, count := range []int{1, 0, 10, 10_000, math.MaxInt, math.MinInt} {
		line := upgradeRunsReview(count)
		if len(line) > 80 || strings.Contains(line, "…") || strings.Contains(line, "...") || !strings.HasSuffix(line, ".") {
			t.Fatal("upgrade review", count, "characters", len(line), line)
		}
	}

}

func assertCustodyLinks(t *testing.T, c *ConfigDir, dir string) {
	t.Helper()
	var err error
	if err = os.Symlink("private", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if data, e := c.read("link"); e != nil || string(data) != "private material" {
		t.Fatal("confined link refused", e)
	}
	if err = c.save("link", []byte("changed")); err == nil {
		t.Fatal("existing file replaced through a link")
	}
	if err = os.Link(filepath.Join(dir, "private"), filepath.Join(dir, "hard")); err != nil {
		t.Fatal(err)
	}
	if data, e := c.read("hard"); e != nil || string(data) != "private material" {
		t.Fatal("private hard link refused", e)
	}
	for _, name := range []string{"unsafe", "public"} {
		if err = os.WriteFile(filepath.Join(dir, name), []byte("data"), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err = c.read(name); err == nil {
			t.Fatal("unsafe file accepted")
		}
	}

}
func assertCustodyProfile(t *testing.T, c *ConfigDir) {
	t.Helper()
	var err error
	profile := Profile{Version: 1, Context: "a", ClusterUID: "one", AdminKey: c.pathFor("admin.key"), AdminCert: c.pathFor("admin.crt"), KnownHosts: c.pathFor("known_hosts")}
	if err = c.saveProfile(profile, "first"); err != nil {
		t.Fatal(err)
	}
	if err = c.checkTarget("a", "one", ""); err != nil {
		t.Fatal("matching profile refused", err)
	}
	wrong := profile
	wrong.ClusterUID = "two"
	if err = c.saveProfile(wrong, "wrong"); err == nil {
		t.Fatal("profile replaced with another UID")
	}
	if err = c.checkTarget("a", "two", ""); err == nil {
		t.Fatal("other cluster accepted")
	}
	b, err := json.Marshal(profile)
	if err != nil || strings.Contains(string(b), "private material") {
		t.Fatal(err)
	}
}

func TestHomeRepositoryAndLinkedAncestorAreAccepted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.Mkdir(filepath.Join(home, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "dotfiles")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(target, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(home, ".config")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(home)
	c, err := OpenConfigDir(filepath.Join(home, ".config", "oberth"))
	if err != nil {
		t.Fatal("ordinary home refused", err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDetailedRefusalLinesStayBounded(t *testing.T) {
	text := described("custody", nil, strings.Repeat("complete path ", 40)).Error()
	for _, line := range strings.Split(text, "\n") {
		if len([]rune(line)) > 120 {
			t.Fatal("unbounded refusal", line)
		}
	}
	if !strings.Contains(text, "credential file") || !strings.Contains(text, "owner-only") {
		t.Fatal(text)
	}
}

func TestLinkedWorkingDirectoryStillRefusesCheckoutCredentials(t *testing.T) {
	checkout := t.TempDir()
	if err := os.Mkdir(filepath.Join(checkout, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "checkout")
	if err := os.Symlink(checkout, alias); err != nil {
		t.Fatal(err)
	}
	t.Chdir(alias)
	c, err := OpenConfigDir(filepath.Join(checkout, "credentials"))
	if err == nil {
		_ = c.Close()
		t.Fatal("physical checkout accepted through linked working directory")
	}
}

func TestLongCredentialPathRemainsCopyable(t *testing.T) {
	path := "/profile/" + strings.Repeat("directory", 20) + "/backup.agekey"
	text := NewProblem("custody", nil, "Copy "+path+" off this machine.").Error()
	if !strings.Contains(text, path) {
		t.Fatal("credential path was split", text)
	}
	for _, line := range strings.Split(text, "\n") {
		if len([]rune(line)) > 120 && line != path {
			t.Fatal("long token shares a wrapped line", line)
		}
	}
}

func TestCredentialFilesStayInsideRoot(t *testing.T) {
	dir, outside := filepath.Join(t.TempDir(), "credentials"), t.TempDir()
	file := filepath.Join(outside, "private")
	if err := os.WriteFile(file, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := OpenConfigDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.create(); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(file, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(dir, file)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{file, relative, "escape"} {
		if _, err = c.read(name); err == nil {
			t.Fatal("credential read escaped root", name)
		}
		if err = c.save(name, []byte("changed")); err == nil {
			t.Fatal("credential write escaped root", name)
		}
	}
	raw, err := os.ReadFile(file)
	if err != nil || string(raw) != "outside" {
		t.Fatal("outside file changed", err)
	}
}
