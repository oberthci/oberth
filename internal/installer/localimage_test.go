package installer

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// --chart with --image skipped the node load entirely, on the reasoning that
// --image names an image the node already has. That is false in the case the
// installer creates itself: it is what makes the kind cluster, so at the moment
// anyone could have loaded an image there was no node to load it into. The Pod
// then sits in ImagePullBackOff against a registry that was never meant to
// serve it.
func TestLocalImageIsLoadedIntoTheNodeWhenTheNodeLacksIt(t *testing.T) {
	t.Parallel()
	pinned := "oberth-registry:5000/oberth@sha256:" + strings.Repeat("a", 64)

	var ran []string
	deps := Deps{
		KindClusterName: "oberth",
		RunCommand: func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
			command := strings.Join(append([]string{name}, args...), " ")
			ran = append(ran, command)
			switch {
			case strings.HasPrefix(command, "docker exec oberth-control-plane ctr --namespace=k8s.io images ls"):
				// Exits 0 and prints nothing when the filter matches nothing.
				return []byte("\n"), nil
			case command == "kind get nodes --name oberth":
				return []byte("oberth-control-plane\n"), nil
			case strings.HasPrefix(command, "docker image inspect"):
				return []byte("localhost:5001/oberth:test\n"), nil
			}
			return nil, nil
		},
	}

	if err := ensureLocalImageInNode(context.Background(), deps, pinned); err != nil {
		t.Fatalf("load failed: %v", err)
	}

	joined := strings.Join(ran, "\n")
	if !strings.Contains(joined, "kind load docker-image") {
		t.Errorf("the image was never loaded into the node:\n%s", joined)
	}
	if !strings.Contains(joined, "images tag --force") {
		t.Errorf("the loaded image was never aliased to the pinned digest:\n%s", joined)
	}
}

// A node that already holds the image must not be loaded again. Reloading is
// slow and, on a repeat install, pointless.
func TestNodeThatAlreadyHasTheImageIsLeftAlone(t *testing.T) {
	t.Parallel()
	pinned := "oberth-registry:5000/oberth@sha256:" + strings.Repeat("b", 64)

	var loaded bool
	deps := Deps{
		KindClusterName: "oberth",
		RunCommand: func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
			command := strings.Join(append([]string{name}, args...), " ")
			if strings.HasPrefix(command, "kind load") {
				loaded = true
			}
			if command == "kind get nodes --name oberth" {
				return []byte("oberth-control-plane\n"), nil
			}
			if strings.Contains(command, "images ls") {
				return []byte(pinned + "\n"), nil // the node has it
			}
			return nil, nil
		},
	}

	if err := ensureLocalImageInNode(context.Background(), deps, pinned); err != nil {
		t.Fatal(err)
	}
	if loaded {
		t.Error("an image the node already holds was loaded again")
	}
}

// Outside kind there is no node to load into, and nothing to do.
func TestNonKindInstallDoesNothing(t *testing.T) {
	t.Parallel()
	var ran int
	deps := Deps{RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) {
		ran++
		return nil, nil
	}}
	if err := ensureLocalImageInNode(context.Background(), deps, "example.invalid/oberth@sha256:"+strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	if ran != 0 {
		t.Errorf("ran %d commands with no kind cluster", ran)
	}
}

// The pinned ref names the repository the cluster resolves by, which is not
// necessarily the repository the local daemon knows. A build that pushes to
// localhost:5001 to obtain a digest, then pins that digest under another name,
// leaves the daemon holding the right image under the wrong name, and a lookup
// by the pinned repository finds nothing.
func TestLocalTagIsFoundByDigestRatherThanByRepository(t *testing.T) {
	t.Parallel()
	digest := "sha256:" + strings.Repeat("e", 64)
	pinned := "oberth-registry:5000/oberth@" + digest

	run := func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
		command := strings.Join(append([]string{name}, args...), " ")
		// Nothing is known under the pinned repository.
		if strings.HasPrefix(command, "docker image inspect") {
			return nil, errors.New("No such image")
		}
		if strings.HasPrefix(command, "docker images") {
			return []byte("localhost:5001/oberth:0.13.31-tz " + digest + "\n" +
				"someone/else:latest sha256:" + strings.Repeat("f", 64) + "\n"), nil
		}
		return nil, nil
	}

	tag, err := localTagForDigest(context.Background(), run, pinned)
	if err != nil {
		t.Fatalf("the image was not found by digest: %v", err)
	}
	if tag != "localhost:5001/oberth:0.13.31-tz" {
		t.Errorf("found %q, want the local tag carrying that digest", tag)
	}
}
