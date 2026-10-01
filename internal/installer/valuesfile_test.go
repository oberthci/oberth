package installer

import (
	"strings"
	"testing"
)

// A deployment with opinions has to express them somewhere. Without a values
// passthrough the only route was to install, then reconcile with a second helm
// command, which is a step that can be forgotten and whose absence is silent:
// the release comes up on chart defaults and looks healthy.
func TestValuesFilesReachHelm(t *testing.T) {
	t.Parallel()

	args := strings.Join(OberthHelmArgs(Config{
		ValuesFiles: []string{"charts/local.yaml", "charts/overrides.yaml"},
	}, OpenBaoResult{}, RekorResult{}), " ")

	for _, want := range []string{"-f charts/local.yaml", "-f charts/overrides.yaml"} {
		if !strings.Contains(args, want) {
			t.Errorf("helm args do not carry %q; got %s", want, args)
		}
	}
}

// Order is the whole meaning of layered values: the last file wins, and helm
// decides that by argument order. Reordering them silently would invert an
// override.
func TestValuesFilesKeepTheOrderTheyWereGivenIn(t *testing.T) {
	t.Parallel()

	args := strings.Join(OberthHelmArgs(Config{
		ValuesFiles: []string{"first.yaml", "second.yaml"},
	}, OpenBaoResult{}, RekorResult{}), " ")

	if strings.Index(args, "first.yaml") > strings.Index(args, "second.yaml") {
		t.Errorf("values files were reordered, inverting which one wins: %s", args)
	}
}

// The installer's own --set flags must still win over a file, or a value the
// installer computed, such as the secret store address it just created, could
// be silently replaced by a stale one in a file.
func TestInstallerComputedValuesWinOverAFile(t *testing.T) {
	t.Parallel()

	args := OberthHelmArgs(Config{
		ValuesFiles: []string{"local.yaml"},
		ImageRef:    "example.invalid/oberth@sha256:" + strings.Repeat("a", 64),
	}, OpenBaoResult{}, RekorResult{})

	joined := strings.Join(args, " ")
	if strings.Index(joined, "-f local.yaml") > strings.Index(joined, "image.ref=") {
		t.Errorf("a values file is applied after the installer's own settings, so it can override them: %s", joined)
	}
}

// Naming no file changes nothing.
func TestNoValuesFileSendsNoFlag(t *testing.T) {
	t.Parallel()

	args := strings.Join(OberthHelmArgs(Config{}, OpenBaoResult{}, RekorResult{}), " ")
	if strings.Contains(args, " -f ") {
		t.Errorf("an install that named no values file sent one: %s", args)
	}
}
