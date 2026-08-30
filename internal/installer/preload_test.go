package installer

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

type recordingRunner struct {
	calls []string
	fail  map[string]error
	out   map[string]string
}

func (r *recordingRunner) run(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
	joined := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, joined)
	for prefix, err := range r.fail {
		if strings.Contains(joined, prefix) {
			return nil, err
		}
	}
	for prefix, out := range r.out {
		if strings.Contains(joined, prefix) {
			return []byte(out), nil
		}
	}
	return nil, nil
}

func preloadDeps(runner *recordingRunner, output *bytes.Buffer) InstallDeps {
	return InstallDeps{Output: output, RunCommand: runner.run}
}

// The pull runs inside the node, not on the host: `kind load` copies from the
// local daemon, which holds the same wrong-architecture image the registry
// does, so it would faithfully copy the problem.
func TestPreloadPullsInsideTheNodeAndRetags(t *testing.T) {
	t.Parallel()
	runner := &recordingRunner{out: map[string]string{"kind get nodes": "oberth-control-plane\n"}}
	var out bytes.Buffer

	err := Preload(context.Background(), preloadDeps(runner, &out), "oberth",
		"docker.io/imresamu/postgis:16-3.4", "postgis/postgis:16-3.4")
	if err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(runner.calls, "\n")
	for _, want := range []string{
		"docker exec oberth-control-plane ctr --namespace=k8s.io images pull docker.io/imresamu/postgis:16-3.4",
		"docker exec oberth-control-plane ctr --namespace=k8s.io images tag --force docker.io/imresamu/postgis:16-3.4 postgis/postgis:16-3.4",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "kind load") {
		t.Fatalf("the host daemon was used as the source:\n%s", joined)
	}
}

// Without --as there is nothing to rename, and renaming anyway would put a
// second name on an image nobody asked to alias.
func TestPreloadWithoutAnAliasOnlyPulls(t *testing.T) {
	t.Parallel()
	runner := &recordingRunner{out: map[string]string{"kind get nodes": "oberth-control-plane\n"}}
	var out bytes.Buffer

	if err := Preload(context.Background(), preloadDeps(runner, &out), "oberth", "alpine:3.20", ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(runner.calls, "\n"), "images tag") {
		t.Fatal("an unaliased preload tagged the image anyway")
	}
}

// A failed pull is the whole point of the command failing: silently exiting
// zero would leave a pod in ErrImageNeverPull with nothing pointing here.
func TestPreloadReportsAFailedPull(t *testing.T) {
	t.Parallel()
	runner := &recordingRunner{
		out:  map[string]string{"kind get nodes": "oberth-control-plane\n"},
		fail: map[string]error{"images pull": errors.New("exit status 1")},
	}
	var out bytes.Buffer

	err := Preload(context.Background(), preloadDeps(runner, &out), "oberth", "alpine:3.20", "")
	if err == nil || !strings.Contains(err.Error(), "alpine:3.20") {
		t.Fatalf("err = %v, want the failing image named", err)
	}
}

func TestPreloadNeedsAnImage(t *testing.T) {
	t.Parallel()
	runner := &recordingRunner{}
	var out bytes.Buffer
	if err := Preload(context.Background(), preloadDeps(runner, &out), "oberth", "  ", ""); err == nil {
		t.Fatal("an empty image reference was accepted")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("commands ran anyway: %v", runner.calls)
	}
}
