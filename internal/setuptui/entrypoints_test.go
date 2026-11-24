package setuptui

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"context"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/installer/fake"
	"github.com/oberthci/oberth/internal/kubeconfig"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/version"
	discoveryfake "k8s.io/client-go/discovery/fake"
	kfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func reviewPlanContext(t *testing.T, name string) *installer.Plan {
	t.Helper()
	client := kfake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "cluster"}})
	fake.DryRunServices(client)
	client.Discovery().(*discoveryfake.FakeDiscovery).FakedServerVersion = &version.Info{GitVersion: "v1.30.0"}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	credentials, err := installer.OpenConfigDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := credentials.Close(); err != nil {
			t.Error(err)
		}
	})
	release, err := installer.LocalRelease(filepath.Join("..", "..", "charts", "oberth"))
	if err != nil {
		t.Fatal(err)
	}
	engine := installer.Engine{Definitions: &fake.Definitions{}, Cluster: kubeconfig.Target{Context: name, Config: &rest.Config{Host: "https://cluster.oberth.ci"}, Client: client}, Charts: &fake.Chart{}, Credentials: credentials, Random: rand.Reader, Now: func() time.Time { return time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC) }, CheckEndpoint: func(context.Context, installer.Profile) error { return nil }}
	plan, err := engine.Prepare(t.Context(), release, installer.Choices{Context: name})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(plan.Close)
	return plan
}
func TestReviewReturnsApprovalForReviewedAnswer(t *testing.T) {
	plan := reviewPlan(t)
	for _, tc := range []struct {
		answer   string
		approved bool
	}{{"y\n", true}, {"Y\n", true}, {"n\n", false}, {"\n", false}, {"unexpected\n", false}} {
		var out bytes.Buffer
		approval, err := Review(t.Context(), terminal(tc.answer, &out, 0, 0), plan)
		if err != nil || (approval == plan.Digest()) != tc.approved {
			t.Fatal(tc.answer, err, approval)
		}
	}
}
func TestProgressUsesEngineReason(t *testing.T) {
	var out bytes.Buffer
	if err := Progress(&out)(installer.Installed); err != nil {
		t.Fatal(err)
	}
	if out.String() != "installed\n" {
		t.Fatal(out.String())
	}
}

func TestCompactReviewShowsClusterUIDAndFitsOneTerminal(t *testing.T) {
	p := reviewPlan(t)
	var out bytes.Buffer
	if _, err := Review(t.Context(), terminal("\n", &out, 0, 0), p); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out.String(), "\n")
	if len(lines) > 24 {
		t.Fatal("review exceeds terminal", len(lines), out.String())
	}
	review, _, _ := strings.Cut(out.String(), "Apply? [y/N]")
	rows := 1
	for _, line := range strings.Split(strings.TrimSuffix(review, "\n"), "\n") {
		for remaining := line; remaining != ""; rows++ {
			remaining = remaining[lineBreak(remaining, 80):]
		}
	}
	if rows > 24 {
		t.Fatal("compact review exceeds one terminal", rows, out.String())
	}
	for _, text := range []string{"Context:", "Cluster: https://cluster.oberth.ci (UID: cluster)", "Namespaces:", "Pod Security: baseline (new namespace)", "Create/update:", "--parallel N", "--no-pod-defaults", "--no-link-local-filter", "--external-store URL", "Files:", "Apply? [y/N]"} {
		if !strings.Contains(strings.ReplaceAll(out.String(), ":\n", ": "), text) {
			t.Fatal("missing approval fact", text, out.String())
		}
	}
}

func reviewPlan(t *testing.T) *installer.Plan { return reviewPlanContext(t, "selected") }
func TestReviewEscapesTerminalControlsBeforeCountingRows(t *testing.T) {
	p := reviewPlanContext(t, "selected\n\n\n\n\x1b[2J")
	var out bytes.Buffer
	if _, err := Review(t.Context(), terminal("n\n", &out, 24, 80), p); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\x1b") || !strings.Contains(out.String(), `selected\n\n\n\n\x1b[2J`) {
		t.Fatal("review contains terminal control bytes", out.String())
	}
	if len(strings.Split(out.String(), "\n")) > 24 || strings.Contains(out.String(), "Page 1") {
		t.Fatal("control characters changed the review's rows")
	}
}

// A real plan one row longer than a 24-row terminal is paged, not refused.
func TestReviewOfTwentyFiveRowsIsPaged(t *testing.T) {
	short := reviewPlan(t)
	extra := 25 - reviewRows(t, short)
	if extra < 1 {
		t.Fatal("compact review fixture leaves no room for a long context")
	}
	p := reviewPlanContext(t, strings.Repeat("x", 80*extra))
	rows := reviewRows(t, p)
	if rows != 25 {
		t.Fatal("fixture must occupy exactly 25 review rows", rows)
	}
	var out bytes.Buffer
	approval, err := Review(t.Context(), terminal("\ny\n", &out, 24, 80), p)
	if err != nil || approval != p.Digest() {
		t.Fatal("long review was not approved", err, out.String())
	}
	if !strings.Contains(out.String(), "Page 1 of 2; press Enter for the next page.") || !strings.HasSuffix(out.String(), "Apply? [y/N] ") {
		t.Fatal("long review was not paged", out.String())
	}
}

func reviewRows(t *testing.T, p *installer.Plan) int {
	t.Helper()
	var rendered bytes.Buffer
	for _, line := range p.Review() {
		if err := writeLine(&rendered, terminalText(line)); err != nil {
			t.Fatal(err)
		}
	}
	rows := 1
	for _, line := range strings.Split(strings.TrimSuffix(rendered.String(), "\n"), "\n") {
		for remaining := line; remaining != ""; rows++ {
			remaining = remaining[lineBreak(remaining, 80):]
		}
	}
	return rows
}
