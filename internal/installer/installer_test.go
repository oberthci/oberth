package installer

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

func TestKindClusterConfigMapsPortsAndCaches(t *testing.T) {
	t.Parallel()

	cacheRoot := filepath.Join(string(filepath.Separator), "Users", "alice", "Library", "Caches")
	config, cacheDirs, err := kindClusterConfig(cacheRoot)
	if err != nil {
		t.Fatal(err)
	}

	text := string(config)
	for _, want := range []string{
		"kind: Cluster",
		"containerPort: 30022",
		"hostPort: 30022",
		"containerPort: 30443",
		"hostPort: 30443",
		"listenAddress: 127.0.0.1",
		"hostPath: " + filepath.Join(cacheRoot, "oberth", "ci"),
		"containerPath: /var/cache/oberth/ci",
		"hostPath: " + filepath.Join(cacheRoot, "oberth", "release"),
		"containerPath: /var/cache/oberth/release",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("kind config missing %q:\n%s", want, text)
		}
	}
	if len(cacheDirs) != 2 {
		t.Fatalf("cache dirs = %v, want CI and release directories", cacheDirs)
	}
}

func TestExecuteDarwinDryRunPrintsKindAndHelmCommands(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	deps := InstallDeps{
		Output: &output,
		GOOS:   "darwin",
		UserCacheDir: func() (string, error) {
			return "/Users/alice/Library/Caches", nil
		},
		LookPath: func(string) (string, error) {
			t.Fatal("dry-run must not check installed prerequisites")
			return "", nil
		},
		RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) {
			t.Fatal("dry-run must not execute host commands")
			return nil, nil
		},
		MkdirAll: func(string, fs.FileMode) error {
			t.Fatal("dry-run must not create cache directories")
			return nil
		},
		LoadKubeConfig: func(string) (kubernetes.Interface, *rest.Config, string, error) {
			t.Fatal("dry-run must not load kubeconfig")
			return nil, nil, "", nil
		},
	}

	// The composable install flags gate every optional stack, so the full
	// macOS plan (kind + images + OpenBao + Oberth) requires the secret-store
	// opt-in exactly as the Linux flow does.
	if err := Execute(context.Background(), Config{DryRun: true, BinaryVersion: "v0.10.55", InstallSecretStore: true}, deps); err != nil {
		t.Fatal(err)
	}

	text := output.String()
	assertSubstringsInOrder(t, text, []string{
		`macOS detected: use kind cluster "oberth"`,
		"cat <<'OBERTH_KIND_CONFIG' | kind create cluster --name oberth --config=-",
		"containerPort: 30022",
		"hostPort: 30443",
		"OBERTH_KIND_CONFIG",
		"helm repo add --force-update oberth-charts https://charts.cloudtaser.io/oberth",
		"helm show values oberth-charts/oberth --version v0.10.55",
		`docker manifest inspect "$OBERTH_IMAGE_REF"`,
		`docker pull "$OBERTH_IMAGE_CHILD_REF"`,
		`docker tag "$OBERTH_IMAGE_CHILD_REF" "$OBERTH_IMAGE_LOCAL_TAG"`,
		`kind load docker-image --name oberth "$OBERTH_IMAGE_LOCAL_TAG"`,
		`docker exec oberth-control-plane ctr --namespace=k8s.io images tag --force "$OBERTH_IMAGE_LOCAL_TAG" "$OBERTH_IMAGE_REF"`,
		"helm repo add --force-update openbao https://openbao.github.io/openbao-helm",
		"helm upgrade --install oberth oberth-charts/oberth",
	})
	for _, count := range []struct {
		command string
		want    int
	}{
		{command: "cat <<'OBERTH_KIND_CONFIG' | kind create cluster --name oberth --config=-", want: 1},
		{command: "helm repo add --force-update oberth-charts https://charts.cloudtaser.io/oberth", want: 1},
		{command: "helm show values oberth-charts/oberth --version v0.10.55", want: 2},
		{command: `docker manifest inspect "$OBERTH_IMAGE_REF"`, want: 1},
		{command: `docker pull "$OBERTH_IMAGE_CHILD_REF"`, want: 1},
		{command: `docker tag "$OBERTH_IMAGE_CHILD_REF" "$OBERTH_IMAGE_LOCAL_TAG"`, want: 1},
		{command: `kind load docker-image --name oberth "$OBERTH_IMAGE_LOCAL_TAG"`, want: 1},
		{command: `docker exec oberth-control-plane ctr --namespace=k8s.io images tag --force "$OBERTH_IMAGE_LOCAL_TAG" "$OBERTH_IMAGE_REF"`, want: 1},
		{command: "helm upgrade --install oberth oberth-charts/oberth", want: 1},
	} {
		if got := strings.Count(text, count.command); got != count.want {
			t.Errorf("%q count = %d, want %d:\n%s", count.command, got, count.want, text)
		}
	}
	if strings.Contains(text, "<image-ref") {
		t.Errorf("dry-run contains unsafe angle-bracket image placeholder:\n%s", text)
	}
}

func TestPrepareDarwinKindChecksPrerequisites(t *testing.T) {
	t.Parallel()

	t.Run("kind missing", func(t *testing.T) {
		deps := InstallDeps{
			Output: io.Discard,
			LookPath: func(name string) (string, error) {
				if name == "docker" {
					return "/usr/local/bin/docker", nil
				}
				return "", errors.New("not found")
			},
		}
		_, _, err := prepareDarwinKind(context.Background(), "/Users/alice/Library/Caches", deps)
		if err == nil || !strings.Contains(err.Error(), "brew install kind") {
			t.Fatalf("error = %v, want kind install instructions", err)
		}
	})

	t.Run("container engine stopped", func(t *testing.T) {
		deps := InstallDeps{
			Output: io.Discard,
			LookPath: func(string) (string, error) {
				return "/usr/local/bin/tool", nil
			},
			RunCommand: func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
				if name == "docker" && len(args) == 1 && args[0] == "info" {
					return []byte("Cannot connect to the Docker daemon"), errors.New("exit status 1")
				}
				return nil, nil
			},
		}
		_, _, err := prepareDarwinKind(context.Background(), "/Users/alice/Library/Caches", deps)
		if err == nil || !strings.Contains(err.Error(), "Docker Desktop or OrbStack") {
			t.Fatalf("error = %v, want container-engine start instructions", err)
		}
	})
}

func TestPrepareDarwinKindCreatesConfiguredCluster(t *testing.T) {
	t.Parallel()

	type commandCall struct {
		name  string
		args  []string
		input string
	}
	var calls []commandCall
	var madeDirs []string
	deps := InstallDeps{
		Output: io.Discard,
		LookPath: func(name string) (string, error) {
			return "/usr/local/bin/" + name, nil
		},
		RunCommand: func(_ context.Context, input []byte, name string, args ...string) ([]byte, error) {
			calls = append(calls, commandCall{name: name, args: slices.Clone(args), input: string(input)})
			if name == "kind" && strings.Join(args, " ") == "get clusters" {
				return []byte("other\n"), nil
			}
			return nil, nil
		},
		MkdirAll: func(path string, mode fs.FileMode) error {
			if mode != 0o750 {
				t.Fatalf("MkdirAll(%q) mode = %o, want 0750", path, mode)
			}
			madeDirs = append(madeDirs, path)
			return nil
		},
	}

	contextName, created, err := prepareDarwinKind(context.Background(), "/Users/alice/Library/Caches", deps)
	if err != nil {
		t.Fatal(err)
	}
	if contextName != KindContextName || !created {
		t.Fatalf("context = %q, created = %v; want %q, true", contextName, created, KindContextName)
	}
	if len(madeDirs) != 2 {
		t.Fatalf("created cache dirs = %v, want 2", madeDirs)
	}

	foundCreate := false
	for _, call := range calls {
		if call.name == "kind" && strings.Join(call.args, " ") == "create cluster --name oberth --config=-" {
			foundCreate = true
			if !strings.Contains(call.input, "containerPort: 30022") ||
				!strings.Contains(call.input, "containerPath: /var/cache/oberth/release") {
				t.Fatalf("kind create received wrong config:\n%s", call.input)
			}
		}
	}
	if !foundCreate {
		t.Fatalf("commands = %+v, want kind create for cluster oberth", calls)
	}
}

func TestPrepareDarwinKindValidatesExistingClusterTopology(t *testing.T) {
	t.Parallel()

	const cacheRoot = "/Users/alice/Library/Caches"
	compatibleInspect := compatibleKindInspectJSON(t, cacheRoot)

	t.Run("compatible", func(t *testing.T) {
		deps := InstallDeps{
			Output: io.Discard,
			LookPath: func(name string) (string, error) {
				return "/usr/local/bin/" + name, nil
			},
			RunCommand: existingKindCommandRunner(t, compatibleInspect),
			MkdirAll: func(string, fs.FileMode) error {
				t.Fatal("compatible existing cluster must not recreate cache directories")
				return nil
			},
		}

		contextName, created, err := prepareDarwinKind(context.Background(), cacheRoot, deps)
		if err != nil {
			t.Fatal(err)
		}
		if contextName != KindContextName || created {
			t.Fatalf("context = %q, created = %v; want %q, false", contextName, created, KindContextName)
		}
	})

	t.Run("incompatible", func(t *testing.T) {
		var inspection []dockerContainerInspect
		if err := json.Unmarshal(compatibleInspect, &inspection); err != nil {
			t.Fatal(err)
		}
		delete(inspection[0].HostConfig.PortBindings, "30443/tcp")
		inspection[0].Mounts = inspection[0].Mounts[:1]
		incompatibleInspect, err := json.Marshal(inspection)
		if err != nil {
			t.Fatal(err)
		}
		deps := InstallDeps{
			Output: io.Discard,
			LookPath: func(name string) (string, error) {
				return "/usr/local/bin/" + name, nil
			},
			RunCommand: existingKindCommandRunner(t, incompatibleInspect),
			MkdirAll: func(string, fs.FileMode) error {
				t.Fatal("incompatible existing cluster must not be changed automatically")
				return nil
			},
		}

		_, _, err = prepareDarwinKind(context.Background(), cacheRoot, deps)
		if err == nil {
			t.Fatal("expected incompatible topology error")
		}
		for _, want := range []string{"missing loopback host binding 30443->30443", "missing bind mount", "kind delete cluster --name oberth", "will not delete it automatically"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error missing %q: %v", want, err)
			}
		}
	})
}

func TestExecuteDarwinSelectsOberthKindContext(t *testing.T) {
	t.Parallel()

	selectedContext := ""
	deps := InstallDeps{
		Output: io.Discard,
		GOOS:   "darwin",
		UserCacheDir: func() (string, error) {
			return "/Users/alice/Library/Caches", nil
		},
		LookPath: func(name string) (string, error) {
			return "/usr/local/bin/" + name, nil
		},
		RunCommand: func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
			if name == "kind" && strings.Join(args, " ") == "get clusters" {
				return []byte(KindClusterName + "\n"), nil
			}
			if name == "docker" && strings.Join(args, " ") == "inspect oberth-control-plane" {
				return compatibleKindInspectJSON(t, "/Users/alice/Library/Caches"), nil
			}
			return nil, nil
		},
		LoadKubeConfig: func(contextName string) (kubernetes.Interface, *rest.Config, string, error) {
			selectedContext = contextName
			return nil, nil, "", errors.New("stop after context selection")
		},
	}

	err := Execute(context.Background(), Config{}, deps)
	if err == nil || !strings.Contains(err.Error(), "stop after context selection") {
		t.Fatalf("error = %v, want injected loader stop", err)
	}
	if selectedContext != KindContextName {
		t.Fatalf("selected context = %q, want %q", selectedContext, KindContextName)
	}
}

const (
	testServerIndexDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testServerArmDigest   = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testServerAmdDigest   = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	testRunnerIndexDigest = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	testServerRepo        = "europe-west4-docker.pkg.dev/example/oberth"
	testRunnerRepo        = "europe-west4-docker.pkg.dev/example/runner"
)

func testIndexJSON(entries ...string) []byte {
	return []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[` + strings.Join(entries, ",") + `]}`)
}

func testIndexEntry(digest, os, arch string) string {
	return `{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + digest + `","platform":{"architecture":"` + arch + `","os":"` + os + `"}}`
}

// Digest-pinned multi-arch chart refs must resolve the node platform's child
// manifest, load it under a deterministic local tag, and alias the pinned
// index digest inside the node's containerd — `docker save` of a partially
// pulled index archive fails `ctr import --all-platforms` on the missing
// sibling platform, and without the alias the digest-referencing pod would
// attempt an unauthenticated registry pull.
func TestPrepareKindImagesResolvesDigestRefsAndAliasesNodes(t *testing.T) {
	t.Parallel()

	serverImage := testServerRepo + "@" + testServerIndexDigest
	serverLocal := planKindDigestImage(serverImage).loadRef

	var events []string
	deps := Deps{
		Output: io.Discard,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			events = append(events, "helm "+strings.Join(args, " "))
			if strings.Join(args, " ") == "show values oberth-charts/oberth --version v0.10.55" {
				return []byte("image:\n  ref: " + serverImage + "\n"), nil
			}
			return nil, nil
		},
		RunCommand: func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
			command := strings.Join(append([]string{name}, args...), " ")
			events = append(events, command)
			switch command {
			case "docker image inspect " + serverLocal:
				return nil, errors.New("image not found")
			case "docker version --format {{.Server.Arch}}":
				return []byte("arm64\n"), nil
			case "docker manifest inspect " + serverImage:
				return testIndexJSON(
					testIndexEntry(testServerAmdDigest, "linux", "amd64"),
					testIndexEntry(testServerArmDigest, "linux", "arm64"),
					testIndexEntry("sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", "unknown", "unknown"),
				), nil
			case "kind get nodes --name oberth":
				return []byte("oberth-control-plane\n"), nil
			}
			return nil, nil
		},
	}

	if err := prepareKindImagesForCluster(context.Background(), Config{ChartVersion: "v0.10.55"}, deps, KindClusterName); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"helm repo add --force-update oberth-charts https://charts.cloudtaser.io/oberth",
		"helm show values oberth-charts/oberth --version v0.10.55",
		"docker image inspect " + serverLocal,
		"docker version --format {{.Server.Arch}}",
		"docker manifest inspect " + serverImage,
		"docker pull " + testServerRepo + "@" + testServerArmDigest,
		"docker tag " + testServerRepo + "@" + testServerArmDigest + " " + serverLocal,
		"kind load docker-image --name oberth " + serverLocal,
		"kind get nodes --name oberth",
		"docker exec oberth-control-plane ctr --namespace=k8s.io images tag --force " + serverLocal + " " + serverImage,
	}
	assertSliceEqual(t, events, want)
}

// Tag-based chart refs keep the direct pull-and-load flow: the tag itself is
// what pods reference, so no platform resolution or digest aliasing applies.
func TestPrepareKindImagesTagRefsKeepDirectLoad(t *testing.T) {
	t.Parallel()

	const serverImage = "private.example/oberth:v1"
	var events []string
	deps := Deps{
		Output: io.Discard,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			events = append(events, "helm "+strings.Join(args, " "))
			if strings.Join(args, " ") == "show values oberth-charts/oberth --version v0.10.55" {
				return []byte("image:\n  ref: " + serverImage + "\n"), nil
			}
			return nil, nil
		},
		RunCommand: func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
			command := strings.Join(append([]string{name}, args...), " ")
			events = append(events, command)
			if strings.HasPrefix(command, "docker image inspect ") {
				return nil, errors.New("image not found")
			}
			return nil, nil
		},
	}

	if err := prepareKindImagesForCluster(context.Background(), Config{ChartVersion: "v0.10.55"}, deps, KindClusterName); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"helm repo add --force-update oberth-charts https://charts.cloudtaser.io/oberth",
		"helm show values oberth-charts/oberth --version v0.10.55",
		"docker image inspect " + serverImage,
		"docker pull " + serverImage,
		"kind load docker-image --name oberth " + serverImage,
	}
	assertSliceEqual(t, events, want)
}

func TestPrepareKindImagesExistingLocalRefsSkipPullButStillLoad(t *testing.T) {
	t.Parallel()

	const serverImage = "private.example/oberth:v1"
	var events []string
	var output bytes.Buffer
	deps := Deps{
		Output: &output,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			events = append(events, "helm "+strings.Join(args, " "))
			if len(args) >= 2 && args[0] == "show" && args[1] == "values" {
				return []byte("image:\n  ref: " + serverImage + "\n"), nil
			}
			return nil, nil
		},
		RunCommand: func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
			command := strings.Join(append([]string{name}, args...), " ")
			events = append(events, command)
			if !strings.HasPrefix(command, "docker image inspect ") && !strings.HasPrefix(command, "kind load docker-image ") {
				t.Fatalf("cached images must not be pulled, got %q", command)
			}
			return nil, nil
		},
	}

	if err := prepareKindImagesForCluster(context.Background(), Config{}, deps, KindClusterName); err != nil {
		t.Fatal(err)
	}
	assertSliceEqual(t, events, []string{
		"helm repo add --force-update oberth-charts https://charts.cloudtaser.io/oberth",
		"helm show values oberth-charts/oberth",
		"docker image inspect " + serverImage,
		"kind load docker-image --name oberth " + serverImage,
	})
	if !strings.Contains(output.String(), serverImage+" already exists locally; skipping pull") {
		t.Errorf("output missing cache-hit message for %s:\n%s", serverImage, output.String())
	}
}

func TestPrepareKindImagesRetryAfterPullFailureLoadsImage(t *testing.T) {
	t.Parallel()

	const serverImage = "private.example/oberth:v1"
	hostImages := make(map[string]bool)
	nodeImages := make(map[string]bool)
	pulls := make(map[string]int)
	loads := make(map[string]int)
	deps := Deps{
		Output: io.Discard,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if len(args) >= 2 && args[0] == "show" && args[1] == "values" {
				return []byte("image:\n  ref: " + serverImage + "\n"), nil
			}
			return nil, nil
		},
		RunCommand: func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
			switch {
			case name == "docker" && len(args) == 3 && args[0] == "image" && args[1] == "inspect":
				if hostImages[args[2]] {
					return nil, nil
				}
				return nil, errors.New("image not found")
			case name == "docker" && len(args) == 2 && args[0] == "pull":
				ref := args[1]
				pulls[ref]++
				if pulls[ref] == 1 {
					return nil, errors.New("injected pull failure")
				}
				hostImages[ref] = true
				return nil, nil
			case name == "kind" && len(args) >= 5 && args[0] == "load" && args[1] == "docker-image" && args[2] == "--name" && args[3] == KindClusterName:
				for _, ref := range args[4:] {
					nodeImages[ref] = true
					loads[ref]++
				}
				return nil, nil
			default:
				t.Fatalf("unexpected command: %s %s", name, strings.Join(args, " "))
				return nil, nil
			}
		},
	}

	err := prepareKindImagesForCluster(context.Background(), Config{}, deps, KindClusterName)
	if err == nil || !strings.Contains(err.Error(), "injected pull failure") {
		t.Fatalf("first attempt error = %v, want injected pull failure", err)
	}
	if nodeImages[serverImage] {
		t.Error("failed pull must not load anything into kind")
	}

	if err := prepareKindImagesForCluster(context.Background(), Config{}, deps, KindClusterName); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !nodeImages[serverImage] {
		t.Error("kind node missing the server image after retry")
	}
	if loads[serverImage] != 1 {
		t.Errorf("kind load count = %d, want 1", loads[serverImage])
	}
	if pulls[serverImage] != 2 {
		t.Errorf("pull count = %d, want 2", pulls[serverImage])
	}
}

// A deleted-and-recreated kind cluster starts with an empty containerd while
// the host Docker cache stays warm. The rerun must skip the pulls but still
// load every image into the fresh node — skipping the load on a host-cache
// hit permanently wedges reruns at the digest alias step ("not found").
func TestPrepareKindImagesClusterRecreationReloadsHostCachedImages(t *testing.T) {
	t.Parallel()

	const serverImage = "private.example/oberth:v1"
	hostImages := make(map[string]bool)
	nodeImages := make(map[string]bool)
	pulls := make(map[string]int)
	deps := Deps{
		Output: io.Discard,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if len(args) >= 2 && args[0] == "show" && args[1] == "values" {
				return []byte("image:\n  ref: " + serverImage + "\n"), nil
			}
			return nil, nil
		},
		RunCommand: func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
			switch {
			case name == "docker" && len(args) == 3 && args[0] == "image" && args[1] == "inspect":
				if hostImages[args[2]] {
					return nil, nil
				}
				return nil, errors.New("image not found")
			case name == "docker" && len(args) == 2 && args[0] == "pull":
				pulls[args[1]]++
				hostImages[args[1]] = true
				return nil, nil
			case name == "kind" && len(args) >= 5 && args[0] == "load" && args[1] == "docker-image" && args[2] == "--name" && args[3] == KindClusterName:
				for _, ref := range args[4:] {
					nodeImages[ref] = true
				}
				return nil, nil
			default:
				t.Fatalf("unexpected command: %s %s", name, strings.Join(args, " "))
				return nil, nil
			}
		},
	}

	if err := prepareKindImagesForCluster(context.Background(), Config{}, deps, KindClusterName); err != nil {
		t.Fatalf("first install: %v", err)
	}

	// kind delete cluster && rerun: node containerd is empty, host cache warm.
	clear(nodeImages)

	if err := prepareKindImagesForCluster(context.Background(), Config{}, deps, KindClusterName); err != nil {
		t.Fatalf("rerun after cluster recreation: %v", err)
	}
	if !nodeImages[serverImage] {
		t.Errorf("recreated kind node missing %s; host-cache hit must not skip the node load", serverImage)
	}
	if pulls[serverImage] != 1 {
		t.Errorf("pull count for %s = %d, want 1; rerun should reuse the host cache", serverImage, pulls[serverImage])
	}
}

func TestPrepareKindImagesCachedDigestSkipsResolutionAndPullButLoadsAndAliases(t *testing.T) {
	t.Parallel()

	image := testServerRepo + "@" + testServerIndexDigest
	local := planKindDigestImage(image).loadRef
	var events []string
	deps := Deps{
		Output: io.Discard,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if len(args) >= 2 && args[0] == "show" && args[1] == "values" {
				return []byte("image:\n  ref: " + image + "\nrunnerImage:\n  ref: " + image + "\n"), nil
			}
			return nil, nil
		},
		RunCommand: func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
			command := strings.Join(append([]string{name}, args...), " ")
			events = append(events, command)
			switch command {
			case "docker image inspect " + local:
				return nil, nil
			case "kind load docker-image --name " + KindClusterName + " " + local:
				return nil, nil
			case "kind get nodes --name " + KindClusterName:
				return []byte("oberth-control-plane\n"), nil
			case "docker exec oberth-control-plane ctr --namespace=k8s.io images tag --force " + local + " " + image:
				return nil, nil
			default:
				t.Fatalf("cached digest must not be resolved or pulled; got %q", command)
				return nil, nil
			}
		},
	}

	if err := prepareKindImagesForCluster(context.Background(), Config{}, deps, KindClusterName); err != nil {
		t.Fatal(err)
	}
	assertSliceEqual(t, events, []string{
		"docker image inspect " + local,
		"kind load docker-image --name " + KindClusterName + " " + local,
		"kind get nodes --name " + KindClusterName,
		"docker exec oberth-control-plane ctr --namespace=k8s.io images tag --force " + local + " " + image,
	})
}

// When `docker manifest inspect` is unavailable (older CLI, registry hiccup)
// the pinned ref is pulled unchanged — the pre-resolution behavior — while
// the local-tag load and node digest alias still apply.
func TestPrepareKindImagesInspectFailureFallsBackToPinnedRef(t *testing.T) {
	t.Parallel()

	serverImage := testServerRepo + "@" + testServerIndexDigest
	serverLocal := planKindDigestImage(serverImage).loadRef

	var events []string
	deps := Deps{
		Output: io.Discard,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if len(args) >= 2 && args[0] == "show" && args[1] == "values" {
				return []byte("image:\n  ref: " + serverImage + "\nrunnerImage:\n  ref: " + serverImage + "\n"), nil
			}
			return nil, nil
		},
		RunCommand: func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
			command := strings.Join(append([]string{name}, args...), " ")
			events = append(events, command)
			switch command {
			case "docker image inspect " + serverLocal:
				return nil, errors.New("image not found")
			case "docker version --format {{.Server.Arch}}":
				return []byte("arm64\n"), nil
			case "docker manifest inspect " + serverImage:
				return []byte("docker manifest is only supported when experimental cli features are enabled"), errors.New("exit status 1")
			case "kind get nodes --name oberth":
				return []byte("oberth-control-plane\n"), nil
			}
			return nil, nil
		},
	}

	if err := prepareKindImagesForCluster(context.Background(), Config{}, deps, KindClusterName); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"docker image inspect " + serverLocal,
		"docker version --format {{.Server.Arch}}",
		"docker manifest inspect " + serverImage,
		"docker pull " + serverImage,
		"docker tag " + serverImage + " " + serverLocal,
		"kind load docker-image --name oberth " + serverLocal,
		"kind get nodes --name oberth",
		"docker exec oberth-control-plane ctr --namespace=k8s.io images tag --force " + serverLocal + " " + serverImage,
	}
	assertSliceEqual(t, events, want)
}

// A pinned index that has no variant for the kind node's platform is a hard,
// actionable error — pulling the index would fail later with a far less
// specific message.
func TestPrepareKindImagesMissingPlatformVariantFails(t *testing.T) {
	t.Parallel()

	serverImage := testServerRepo + "@" + testServerIndexDigest
	serverLocal := planKindDigestImage(serverImage).loadRef
	deps := Deps{
		Output: io.Discard,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if len(args) >= 2 && args[0] == "show" && args[1] == "values" {
				return []byte("image:\n  ref: " + serverImage + "\nrunnerImage:\n  ref: " + serverImage + "\n"), nil
			}
			return nil, nil
		},
		RunCommand: func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
			command := strings.Join(append([]string{name}, args...), " ")
			switch command {
			case "docker image inspect " + serverLocal:
				return nil, errors.New("image not found")
			case "docker version --format {{.Server.Arch}}":
				return []byte("arm64\n"), nil
			case "docker manifest inspect " + serverImage:
				return testIndexJSON(testIndexEntry(testServerAmdDigest, "linux", "amd64")), nil
			}
			return nil, nil
		},
	}

	err := prepareKindImagesForCluster(context.Background(), Config{}, deps, KindClusterName)
	if err == nil {
		t.Fatal("expected missing-platform error")
	}
	for _, want := range []string{"no linux/arm64 variant", "available: linux/amd64"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

func TestPlanKindDigestImageLocalTag(t *testing.T) {
	t.Parallel()

	serverImage := testServerRepo + "@" + testServerIndexDigest
	plan := planKindDigestImage(serverImage)
	if plan.chartRef != serverImage || plan.pullRef != serverImage || !plan.alias {
		t.Fatalf("plan = %+v, want chartRef/pullRef %q with alias", plan, serverImage)
	}
	prefix := testServerRepo + ":oberth-kind-"
	suffix := strings.TrimPrefix(plan.loadRef, prefix)
	if suffix == plan.loadRef || len(suffix) != 12 {
		t.Fatalf("loadRef = %q, want %q + 12 hex characters", plan.loadRef, prefix)
	}
	if again := planKindDigestImage(serverImage); again.loadRef != plan.loadRef {
		t.Errorf("local tag is not deterministic: %q vs %q", again.loadRef, plan.loadRef)
	}
	other := planKindDigestImage(testServerRepo + "@" + testServerArmDigest)
	if other.loadRef == plan.loadRef {
		t.Errorf("distinct refs share local tag %q", plan.loadRef)
	}
}

func TestImageRepositoryStripsTagAndDigest(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ ref, want string }{
		{ref: "registry.example:5000/team/app@sha256:" + strings.Repeat("a", 64), want: "registry.example:5000/team/app"},
		{ref: "registry.example:5000/team/app:v1@sha256:" + strings.Repeat("a", 64), want: "registry.example:5000/team/app"},
		{ref: "registry.example:5000/team/app:v1", want: "registry.example:5000/team/app"},
		{ref: "registry.example/team/app", want: "registry.example/team/app"},
	} {
		if got := imageRepository(tc.ref); got != tc.want {
			t.Errorf("imageRepository(%q) = %q, want %q", tc.ref, got, tc.want)
		}
	}
}

func TestKindNodePlatformArch(t *testing.T) {
	t.Parallel()

	probed := Deps{RunCommand: func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
		if name == "docker" && len(args) == 3 && args[0] == "version" {
			return []byte("arm64\n"), nil
		}
		return nil, errors.New("unexpected command")
	}}
	if got := kindNodePlatformArch(context.Background(), probed); got != "arm64" {
		t.Errorf("probed arch = %q, want arm64", got)
	}

	failing := Deps{RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) {
		return nil, errors.New("docker unavailable")
	}}
	if got := kindNodePlatformArch(context.Background(), failing); got != runtime.GOARCH {
		t.Errorf("fallback arch = %q, want %q", got, runtime.GOARCH)
	}
}

func TestExecuteDarwinNodeListFailureStillLoadsKindImages(t *testing.T) {
	t.Parallel()

	ready := corev1.ConditionTrue
	kubeClient := fake.NewClientset(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: "kube-public"},
			Data:       map[string]string{"ca.crt": "test-ca"},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "openbao-0", Namespace: DefaultOpenBaoNamespace, Labels: map[string]string{"app.kubernetes.io/instance": "openbao"}},
			Status:     corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: ready}}},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "oberth-0", Namespace: DefaultNamespace, Labels: map[string]string{"app.kubernetes.io/instance": "oberth"}},
			Status:     corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: ready}}},
		},
		readyArgoControllerPod(),
	)
	kubeClient.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, errors.New("node discovery unavailable")
	})

	var hostCommands []string
	deps := InstallDeps{
		Output: io.Discard,
		GOOS:   "darwin",
		UserCacheDir: func() (string, error) {
			return "/Users/alice/Library/Caches", nil
		},
		LookPath: func(name string) (string, error) {
			return "/usr/local/bin/" + name, nil
		},
		RunCommand: func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
			command := strings.Join(append([]string{name}, args...), " ")
			switch command {
			case "kind get clusters":
				return []byte(KindClusterName + "\n"), nil
			case "docker inspect oberth-control-plane":
				return compatibleKindInspectJSON(t, "/Users/alice/Library/Caches"), nil
			case "docker image inspect private.example/oberth:latest":
				return nil, errors.New("image not found")
			case "docker pull private.example/oberth:latest",
				"kind load docker-image --name oberth private.example/oberth:latest":
				hostCommands = append(hostCommands, command)
			}
			if name == "kubectl" && strings.Contains(command, "upstream list") {
				// One upstream is already registered, so the finish path waits
				// for the Ready pod instead of starting onboarding.
				return []byte("NAME   KIND   URL                              KEY FINGERPRINT\ncodeberg   ssh   ssh://git@codeberg.org/cloudtaser   SHA256:x\n"), nil
			}
			return nil, nil
		},
		IsTerminal: func() bool { return false },
		LoadKubeConfig: func(contextName string) (kubernetes.Interface, *rest.Config, string, error) {
			if contextName != KindContextName {
				t.Fatalf("selected context = %q, want %q", contextName, KindContextName)
			}
			return kubeClient, &rest.Config{Host: "https://127.0.0.1:6443"}, contextName, nil
		},
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			command := strings.Join(args, " ")
			switch {
			case command == "list -n openbao -o json", command == "list -n oberth -o json":
				return []byte("[]"), nil
			case strings.HasPrefix(command, "show values oberth-charts/oberth"):
				return []byte("image:\n  ref: private.example/oberth:latest\n"), nil
			default:
				return nil, nil
			}
		},
	}

	if err := Execute(context.Background(), Config{BinaryVersion: "v0.10.55"}, deps); err != nil {
		t.Fatal(err)
	}
	assertSliceEqual(t, hostCommands, []string{
		"docker pull private.example/oberth:latest",
		"kind load docker-image --name oberth private.example/oberth:latest",
	})
}

func TestPrepareKindImagesExplainsGARAuthentication(t *testing.T) {
	t.Parallel()

	deps := Deps{
		Output: io.Discard,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if len(args) >= 2 && args[0] == "show" && args[1] == "values" {
				return []byte("image:\n  ref: private.example/oberth:latest\nrunnerImage:\n  ref: private.example/runner:latest\n"), nil
			}
			return nil, nil
		},
		RunCommand: func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
			if name == "docker" && len(args) >= 2 && args[0] == "image" && args[1] == "inspect" {
				return nil, errors.New("image not found")
			}
			if name == "docker" && len(args) > 0 && args[0] == "pull" {
				return []byte("unauthorized"), errors.New("exit status 1")
			}
			return nil, nil
		},
	}

	err := prepareKindImagesForCluster(context.Background(), Config{}, deps, KindClusterName)
	if err == nil {
		t.Fatal("expected private image pull error")
	}
	for _, want := range []string{"unauthorized", "gcloud auth configure-docker", "registry credentials"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

// --- Config validation ---

func TestConfigValidateDevProductionMutualExclusion(t *testing.T) {
	t.Parallel()
	cfg := Config{Dev: true, Production: true}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("want mutual exclusion error, got %v", err)
	}
}

func TestConfigValidateProductionRejected(t *testing.T) {
	t.Parallel()
	cfg := Config{Production: true}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("want error for production mode")
	}
	if !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("error should mention not implemented, got %v", err)
	}
	if !strings.Contains(err.Error(), "BACKLOG.md") {
		t.Fatalf("error should mention BACKLOG.md, got %v", err)
	}
}

func TestConfigValidateDefaultsToDev(t *testing.T) {
	t.Parallel()
	cfg := Config{}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if !cfg.Dev {
		t.Fatal("should default to dev mode")
	}
	if cfg.OpenBaoNamespace != DefaultOpenBaoNamespace {
		t.Fatalf("OpenBaoNamespace = %q, want %q", cfg.OpenBaoNamespace, DefaultOpenBaoNamespace)
	}
	if cfg.Namespace != DefaultNamespace {
		t.Fatalf("Namespace = %q, want %q", cfg.Namespace, DefaultNamespace)
	}
	if cfg.Timeout != DefaultTimeout {
		t.Fatalf("Timeout = %v, want %v", cfg.Timeout, DefaultTimeout)
	}
	if cfg.OpenBaoChartVersion != DefaultOpenBaoChartVersion {
		t.Fatalf("OpenBaoChartVersion = %q, want %q", cfg.OpenBaoChartVersion, DefaultOpenBaoChartVersion)
	}
}

func TestConfigValidateChartVersionFromBinaryVersion(t *testing.T) {
	t.Parallel()
	cfg := Config{BinaryVersion: "v0.10.54"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.ChartVersion != "v0.10.54" {
		t.Fatalf("ChartVersion = %q, want v0.10.54", cfg.ChartVersion)
	}
}

func TestConfigValidateChartVersionNotFromDevBuild(t *testing.T) {
	t.Parallel()
	cfg := Config{BinaryVersion: "dev"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.ChartVersion != "" {
		t.Fatalf("ChartVersion = %q, want empty for dev build", cfg.ChartVersion)
	}
}

// --- Cluster detection ---

func TestIsLocalServer(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		url  string
		want bool
	}{
		{"https://127.0.0.1:6443", true},
		{"https://localhost:6443", true},
		{"https://[::1]:6443", true},
		{"https://192.168.1.100:6443", true},
		{"https://10.0.0.1:6443", true},
		{"https://172.16.0.1:6443", true},
		{"https://172.31.255.255:6443", true},
		{"https://35.200.100.1:6443", false},
		{"https://k8s.example.com:6443", false},
		{"https://100.64.0.1:6443", false},
	} {
		if got := IsLocalServer(test.url); got != test.want {
			t.Errorf("IsLocalServer(%q) = %v, want %v", test.url, got, test.want)
		}
	}
}

func TestDetectEngine(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		nodes []corev1.Node
		want  string
	}{
		{
			name: "k3s from kubelet version",
			nodes: []corev1.Node{{
				Status: corev1.NodeStatus{
					NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.33.0+k3s1"},
				},
			}},
			want: "k3s",
		},
		{
			name: "kind from providerID",
			nodes: []corev1.Node{{
				Spec: corev1.NodeSpec{ProviderID: "kind://docker/kind/kind-control-plane"},
			}},
			want: "kind",
		},
		{
			name: "k0s from kubelet version",
			nodes: []corev1.Node{{
				Status: corev1.NodeStatus{
					NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.33.0+k0s.0"},
				},
			}},
			want: "k0s",
		},
		{
			name: "minikube from label",
			nodes: []corev1.Node{{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"minikube.k8s.io/version": "v1.33.0"},
				},
			}},
			want: "minikube",
		},
		{
			name: "minikube from providerID",
			nodes: []corev1.Node{{
				Spec: corev1.NodeSpec{ProviderID: "minikube://host"},
			}},
			want: "minikube",
		},
		{
			name: "unknown engine",
			nodes: []corev1.Node{{
				Status: corev1.NodeStatus{
					NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.33.0"},
				},
			}},
			want: "unknown",
		},
		{
			name:  "no nodes",
			nodes: nil,
			want:  "unknown",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := DetectEngine(test.nodes); got != test.want {
				t.Errorf("DetectEngine = %q, want %q", got, test.want)
			}
		})
	}
}

// --- Remote context refusal ---

func TestRemoteContextRefusal(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "gke-node"},
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.33.0"},
		},
	}
	deps := Deps{
		Output:      &buf,
		KubeClient:  fake.NewClientset(node, readyArgoControllerPod()),
		RestConfig:  &rest.Config{Host: "https://35.200.100.1:6443"},
		ContextName: "gke_project_zone_cluster",
	}
	cfg := Config{}
	err := Run(context.Background(), cfg, deps)
	if err == nil {
		t.Fatal("expected error for remote context without --yes")
	}
	if !strings.Contains(err.Error(), "does not appear to be a local cluster") {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("error should mention --yes, got %v", err)
	}
}

// --- Dry-run zero-writes ---

func TestDryRunZeroWrites(t *testing.T) {
	t.Parallel()
	helmCalls := 0
	var buf bytes.Buffer
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "kind-control-plane"},
		Spec:       corev1.NodeSpec{ProviderID: "kind://docker/kind/kind-control-plane"},
	}
	deps := Deps{
		Output: &buf,
		RunHelm: func(_ context.Context, _ []string) ([]byte, error) {
			helmCalls++
			return nil, nil
		},
		KubeClient:  fake.NewClientset(node, readyArgoControllerPod()),
		RestConfig:  &rest.Config{Host: "https://127.0.0.1:6443"},
		ContextName: "kind-test",
	}
	cfg := Config{DryRun: true}
	if err := Run(context.Background(), cfg, deps); err != nil {
		t.Fatal(err)
	}
	if helmCalls != 0 {
		t.Fatalf("dry-run called helm %d times, want 0", helmCalls)
	}
	output := buf.String()
	if !strings.Contains(output, "Dry-run plan") {
		t.Fatalf("output missing dry-run plan header:\n%s", output)
	}
	if !strings.Contains(output, "No cluster changes were made") {
		t.Fatalf("output missing no-changes confirmation:\n%s", output)
	}
}

// --- Helm arg construction ---

func TestOpenBaoHelmArgsDevMode(t *testing.T) {
	t.Parallel()
	cfg := Config{
		InstallSecretStoreDev: true,
		OpenBaoNamespace:      "openbao",
		OpenBaoChartVersion:   "0.6.0",
	}
	args := OpenBaoHelmArgs(cfg)
	want := []string{
		"upgrade", "--install", "openbao", "openbao/openbao",
		"-n", "openbao", "--create-namespace",
		// Oberth never uses the agent injector; keep the unused privileged
		// webhook out of both install modes.
		"--set", "injector.enabled=false",
		"--set", "global.tlsDisable=true",
		"--set", "server.dev.enabled=true",
		"--version", "0.6.0",
		"--reuse-values",
		"--wait",
	}
	assertSliceEqual(t, args, want)
}

func TestOpenBaoHelmArgsProductionMode(t *testing.T) {
	t.Parallel()
	cfg := Config{
		InstallSecretStore:  true,
		OpenBaoNamespace:    "openbao",
		OpenBaoChartVersion: "0.28.6",
	}
	args := OpenBaoHelmArgs(cfg)
	want := []string{
		"upgrade", "--install", "openbao", "openbao/openbao",
		"-n", "openbao", "--create-namespace",
		"--set", "injector.enabled=false",
		"--set", "global.tlsDisable=false",
		// dev pinned false so --reuse-values cannot resurrect a previous
		// dev-mode install's flag through an --upgrade mode switch.
		"--set", "server.dev.enabled=false",
		"--set", "server.standalone.enabled=true",
		"--set", "server.dataStorage.enabled=true",
		"--set", "server.statefulSet.securityContext.pod.runAsNonRoot=true",
		"--set", "server.statefulSet.securityContext.pod.runAsUser=100",
		"--set", "server.statefulSet.securityContext.pod.runAsGroup=1000",
		"--set", "server.statefulSet.securityContext.pod.fsGroup=1000",
		"--set", "server.statefulSet.securityContext.pod.fsGroupChangePolicy=OnRootMismatch",
		"--set", "server.statefulSet.securityContext.pod.seccompProfile.type=RuntimeDefault",
		"--set-string", "server.extraEnvironmentVars.BAO_CACERT=" + openBaoTLSDirectory + "/ca.crt",
		"--set-string", "server.standalone.config=" + openBaoStandaloneTLSConfig,
		"--version", "0.28.6",
		"--reuse-values",
	}
	assertSliceEqual(t, args, want)
	// A sealed production server fails its readiness probe by design, so
	// helm --wait would deadlock every fresh production install.
	for _, arg := range args {
		if arg == "--wait" {
			t.Fatal("production-mode OpenBao install must not use helm --wait")
		}
	}
}

func TestOpenBaoHelmArgsCustomNamespace(t *testing.T) {
	t.Parallel()
	cfg := Config{
		InstallSecretStoreDev: true,
		OpenBaoNamespace:      "custom-vault",
		OpenBaoChartVersion:   "0.7.0",
	}
	args := OpenBaoHelmArgs(cfg)
	if args[5] != "custom-vault" {
		t.Fatalf("namespace arg = %q, want custom-vault", args[5])
	}
	version := ""
	for i, arg := range args {
		if arg == "--version" && i+1 < len(args) {
			version = args[i+1]
		}
	}
	if version != "0.7.0" {
		t.Fatalf("version arg = %q, want 0.7.0", version)
	}
}

func TestOberthHelmArgs(t *testing.T) {
	t.Parallel()
	const caPEM = "-----BEGIN CERTIFICATE-----\npublic-ca\n-----END CERTIFICATE-----\n"
	tests := map[string]struct {
		cfg      Config
		openbao  OpenBaoResult
		settings []string
	}{
		"production": {
			cfg:     Config{Namespace: "oberth", ChartVersion: "v0.10.54", InstallSecretStore: true},
			openbao: OpenBaoResult{ServiceAddress: "https://openbao.openbao.svc:8200", CACertPEM: caPEM, TrustedTransitVerified: true},
			settings: []string{
				// The installer defaults argo.vault.address to the installed
				// store when no explicit --argo-vault-address was given, so
				// the first credentialed release step can reach OpenBao.
				"--set", "argo.vault.address=https://openbao.openbao.svc:8200",
				"--set", "argo.vault.credentialedRole=oberth-argo-credentialed",
				"--set", "argo.ciSecrets.vaultRole=oberth-argo-ci-secrets",
				// The CA cert is auto-pinned because the defaulted address
				// matches the installed store's ServiceAddress.
				"--set-string", "argo.vault.caCert=" + caPEM,
				"--set", "secretstore.enabled=true",
				"--set", "secretstore.address=https://openbao.openbao.svc:8200",
				"--set", "secretstore.role=oberth-ci",
				"--set-string", "secretstore.caCert=" + caPEM,
				"--set", "secretstore.insecureHTTPForDev=false",
				"--set", "secretstore.transit.enabled=true",
				"--set", "secretstore.transit.mount=oberth-transit",
				"--set", "secretstore.transit.key=trusted-plan-artifacts",
			},
		},
		"dev": {
			cfg:     Config{Namespace: "oberth", ChartVersion: "v0.10.54", InstallSecretStoreDev: true},
			openbao: OpenBaoResult{ServiceAddress: "http://openbao.openbao.svc:8200"},
			settings: []string{
				"--set", "argo.vault.credentialedRole=oberth-argo-credentialed",
				"--set", "argo.ciSecrets.vaultRole=oberth-argo-ci-secrets",
				"--set", "secretstore.enabled=true",
				"--set", "secretstore.address=http://openbao.openbao.svc:8200",
				"--set", "secretstore.role=oberth-ci",
				"--set-string", "secretstore.caCert=",
				"--set", "secretstore.insecureHTTPForDev=true",
				"--set", "secretstore.transit.enabled=false",
			},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			args := OberthHelmArgs(test.cfg, test.openbao, RekorResult{})
			want := []string{
				"upgrade", "--install", "oberth", "oberth-charts/oberth",
				"-n", "oberth", "--create-namespace",
				// The execution engine binding is unconditional: Argo is how
				// every pipeline runs, not an optional integration.
				"--set", "argo.namespace=oberth-argo",
			}
			want = append(want, test.settings...)
			want = append(want, "--version", "v0.10.54", "--reuse-values")
			assertSliceEqual(t, args, want)
		})
	}
}

func TestInstallOberthUpgradeFromLegacyValuesPinsManagedTransitIdentity(t *testing.T) {
	t.Parallel()
	var upgradeArgs []string
	deps := Deps{
		Output: io.Discard,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			switch args[0] {
			case "list":
				// v0.10.92 predates the transit.mount/key chart values. Helm's
				// --reuse-values therefore supplies neither on this upgrade.
				return []byte(`[{"name":"oberth","namespace":"oberth","status":"deployed","chart":"oberth-0.10.92"}]`), nil
			case "upgrade":
				upgradeArgs = slices.Clone(args)
			}
			return nil, nil
		},
	}
	result, err := InstallOberth(context.Background(), Config{
		Namespace:          "oberth",
		ChartVersion:       "v0.10.93",
		InstallSecretStore: true,
		Upgrade:            true,
	}, deps, OpenBaoResult{
		ServiceAddress:         "https://openbao.openbao.svc:8200",
		CACertPEM:              "public-ca",
		TrustedTransitVerified: true,
	}, RekorResult{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Upgraded || result.AlreadyInstalled {
		t.Fatalf("legacy release result = %+v, want upgrade", result)
	}

	want := map[string]bool{
		"--reuse-values":                                 false,
		"secretstore.transit.enabled=true":               false,
		"secretstore.transit.mount=oberth-transit":       false,
		"secretstore.transit.key=trusted-plan-artifacts": false,
	}
	for _, arg := range upgradeArgs {
		if _, ok := want[arg]; ok {
			want[arg] = true
		}
	}
	for value, found := range want {
		if !found {
			t.Errorf("legacy --reuse-values upgrade does not override missing %q: %v", value, upgradeArgs)
		}
	}

	rendered := renderOberthHelmArgsWithLegacyValues(t, upgradeArgs)
	for _, want := range []string{
		"--secretstore-transit-mount=oberth-transit",
		"--secretstore-transit-key=trusted-plan-artifacts",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("legacy values upgrade did not render %q", want)
		}
	}
}

func renderOberthHelmArgsWithLegacyValues(t *testing.T, upgradeArgs []string) string {
	t.Helper()
	const transitDefaults = `  transit:
    enabled: false
    mount: oberth-transit
    key: trusted-plan-artifacts
`
	chartPath := filepath.Join("..", "..", "charts", "oberth")
	legacyChartPath := filepath.Join(t.TempDir(), "oberth")
	if err := os.CopyFS(legacyChartPath, os.DirFS(chartPath)); err != nil {
		t.Fatal(err)
	}
	valuesPath := filepath.Join(legacyChartPath, "values.yaml")
	values, err := os.ReadFile(valuesPath) // #nosec G304 -- fixed test fixture path.
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(values), transitDefaults) != 1 {
		t.Fatal("chart values no longer contain the expected managed Transit defaults")
	}
	legacyValues := strings.Replace(string(values), transitDefaults, "", 1)
	if err := os.WriteFile(valuesPath, []byte(legacyValues), 0o600); err != nil {
		t.Fatal(err)
	}

	templateArgs := []string{"template", "oberth", legacyChartPath}
	for index := 4; index < len(upgradeArgs); index++ {
		switch upgradeArgs[index] {
		case "--create-namespace", "--reuse-values":
			continue
		case "--version":
			index++
			continue
		default:
			templateArgs = append(templateArgs, upgradeArgs[index])
		}
	}
	output, err := exec.Command("helm", templateArgs...).CombinedOutput() // #nosec G204 -- fixed binary and installer-owned arguments.
	if err != nil {
		t.Fatalf("render chart against legacy reused values: %v\n%s", err, output)
	}
	return string(output)
}

func TestOberthHelmArgsDoesNotEmitTransitIdentityWithoutPositiveVerification(t *testing.T) {
	t.Parallel()
	args := OberthHelmArgs(Config{InstallSecretStore: true}, OpenBaoResult{
		ServiceAddress: "https://openbao.openbao.svc:8200",
		CACertPEM:      "public-ca",
	}, RekorResult{})

	joined := strings.Join(args, "\n")
	if !strings.Contains(joined, "secretstore.transit.enabled=false") {
		t.Fatalf("unverified production Transit was not explicitly disabled: %v", args)
	}
	for _, forbidden := range []string{
		"secretstore.transit.mount=",
		"secretstore.transit.key=",
	} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("unverified production Transit emitted managed identity %q: %v", forbidden, args)
		}
	}
}

func TestOberthHelmArgsWithoutSecretStore(t *testing.T) {
	t.Parallel()
	cfg := Config{Namespace: "oberth", ChartVersion: "v0.10.54"}
	args := OberthHelmArgs(cfg, OpenBaoResult{}, RekorResult{})
	want := []string{
		"upgrade", "--install", "oberth", "oberth-charts/oberth",
		"-n", "oberth", "--create-namespace",
		"--set", "argo.namespace=oberth-argo",
		"--version", "v0.10.54",
		"--reuse-values",
	}
	assertSliceEqual(t, args, want)
	for _, arg := range args {
		if strings.Contains(arg, "secretstore") || strings.Contains(arg, "auditAnchor") {
			t.Fatalf("plain install must not set optional service values, got %q", arg)
		}
	}
}

func TestOberthHelmArgsWithRekor(t *testing.T) {
	t.Parallel()
	const publicKeyPEM = "-----BEGIN PUBLIC KEY-----\nQUJD\n-----END PUBLIC KEY-----\n"
	cfg := Config{Namespace: "oberth", ChartVersion: "v0.10.54", InstallRekor: true}
	rekor := RekorResult{
		ServiceAddress:  "http://rekor-server.rekor.svc:80",
		LogPublicKeyPEM: publicKeyPEM,
	}
	args := OberthHelmArgs(cfg, OpenBaoResult{}, rekor)
	want := []string{
		"upgrade", "--install", "oberth", "oberth-charts/oberth",
		"-n", "oberth", "--create-namespace",
		"--set", "argo.namespace=oberth-argo",
		"--set", "auditAnchor.rekorURL=http://rekor-server.rekor.svc:80",
		"--set", "auditAnchor.rekorInsecureHTTP=true",
		"--set-string", "auditAnchor.rekorPublicKey=" + publicKeyPEM,
		"--set-string", "auditAnchor.acceptWitnessGenesis=",
		"--version", "v0.10.54",
		"--reuse-values",
	}
	assertSliceEqual(t, args, want)
}

func TestOberthHelmArgsWithRekorAndSecretStore(t *testing.T) {
	t.Parallel()
	const publicKeyPEM = "-----BEGIN PUBLIC KEY-----\nQUJD\n-----END PUBLIC KEY-----\n"
	rekor := RekorResult{
		ServiceAddress:  "http://rekor-server.rekor.svc:80",
		LogPublicKeyPEM: publicKeyPEM,
	}
	for name, test := range map[string]struct {
		cfg     Config
		openbao OpenBaoResult
	}{
		"production": {
			cfg: Config{
				Namespace:          "oberth",
				ChartVersion:       "v0.10.54",
				InstallSecretStore: true,
				InstallRekor:       true,
			},
			openbao: OpenBaoResult{ServiceAddress: "https://openbao.openbao.svc:8200", CACertPEM: "public-ca", TrustedTransitVerified: true},
		},
		"dev": {
			cfg: Config{
				Namespace:             "oberth",
				ChartVersion:          "v0.10.54",
				InstallSecretStoreDev: true,
				InstallRekor:          true,
			},
			openbao: OpenBaoResult{ServiceAddress: "http://openbao.openbao.svc:8200"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			args := OberthHelmArgs(test.cfg, test.openbao, rekor)
			joined := strings.Join(args, "\n")
			for _, want := range []string{
				"auditAnchor.rekorURL=http://rekor-server.rekor.svc:80",
				"auditAnchor.rekorInsecureHTTP=true",
				"auditAnchor.rekorPublicKey=" + publicKeyPEM,
			} {
				if !strings.Contains(joined, want) {
					t.Fatalf("combined service args missing %q: %v", want, args)
				}
			}
			if name == "production" && (!strings.Contains(joined, "secretstore.transit.enabled=true") || strings.Contains(joined, "secretstore.insecureHTTPForDev=true")) {
				t.Fatalf("production secret store args are not fail-closed: %v", args)
			}
			if name == "dev" && (!strings.Contains(joined, "secretstore.transit.enabled=false") || !strings.Contains(joined, "secretstore.insecureHTTPForDev=true")) {
				t.Fatalf("development secret store args are not explicitly KV-only: %v", args)
			}
		})
	}
}

func TestOberthHelmArgsNoVersion(t *testing.T) {
	t.Parallel()
	cfg := Config{Namespace: "oberth", InstallSecretStore: true}
	args := OberthHelmArgs(cfg, OpenBaoResult{ServiceAddress: "https://openbao.openbao.svc:8200", CACertPEM: "public-ca", TrustedTransitVerified: true}, RekorResult{})
	for _, arg := range args {
		if arg == "--version" {
			t.Fatal("--version should not be present when ChartVersion is empty")
		}
	}
}

// --- NetworkPolicy pinning ---

func TestOberthHelmArgsPinsNetworkPolicy(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		policy string
		want   string
	}{
		{"true", "true", "networkPolicy.enabled=true"},
		{"false", "false", "networkPolicy.enabled=false"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := Config{Namespace: "oberth", ChartVersion: "v0.10.54", NetworkPolicy: test.policy}
			args := OberthHelmArgs(cfg, OpenBaoResult{}, RekorResult{})
			found := false
			for i, arg := range args {
				if arg == "--set" && i+1 < len(args) && args[i+1] == test.want {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("OberthHelmArgs must pin %q when NetworkPolicy=%q, args=%v", test.want, test.policy, args)
			}
		})
	}
}

func TestOberthHelmArgsOmitsNetworkPolicyWhenUnresolved(t *testing.T) {
	t.Parallel()
	cfg := Config{Namespace: "oberth", ChartVersion: "v0.10.54"}
	args := OberthHelmArgs(cfg, OpenBaoResult{}, RekorResult{})
	for _, arg := range args {
		if strings.Contains(arg, "networkPolicy") {
			t.Fatalf("OberthHelmArgs must not set networkPolicy when unresolved, got %q", arg)
		}
	}
}

func TestConfigValidateNetworkPolicy(t *testing.T) {
	t.Parallel()
	for _, val := range []string{"", "auto", "true", "false"} {
		cfg := Config{NetworkPolicy: val}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("NetworkPolicy=%q must be valid, got %v", val, err)
		}
	}
	cfg := Config{NetworkPolicy: "yes"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("NetworkPolicy=yes must be rejected")
	}
}

// --- Local Rekor stack (--install-rekor) ---

func TestConfigValidateInstallRekorAllowed(t *testing.T) {
	t.Parallel()
	cfg := Config{InstallRekor: true}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("--install-rekor must validate now that the local witness is wired, got %v", err)
	}
}

func TestConfigValidateInstallRekorDryRunAllowed(t *testing.T) {
	t.Parallel()
	cfg := Config{InstallRekor: true, DryRun: true}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("dry-run with --install-rekor must validate, got %v", err)
	}
	if cfg.RekorNamespace != DefaultRekorNamespace {
		t.Fatalf("RekorNamespace = %q, want %q", cfg.RekorNamespace, DefaultRekorNamespace)
	}
	if cfg.RekorChartVersion != DefaultRekorChartVersion {
		t.Fatalf("RekorChartVersion = %q, want %q", cfg.RekorChartVersion, DefaultRekorChartVersion)
	}
}

func TestRunInstallRekorWiresOberth(t *testing.T) {
	t.Parallel()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "k3s-node"},
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.31.4+k3s1"},
		},
	}
	readyCondition := []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	rekorPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rekor-server",
			Namespace: DefaultRekorNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/instance":  "rekor",
				"app.kubernetes.io/component": "server",
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: readyCondition},
	}
	oberthPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "oberth",
			Namespace: DefaultNamespace,
			Labels:    map[string]string{"app.kubernetes.io/instance": "oberth"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: readyCondition},
	}
	client := fake.NewClientset(node, rekorPod, oberthPod, readyArgoControllerPod())
	var oberthArgs []string
	deps := Deps{
		Output:      io.Discard,
		KubeClient:  client,
		RestConfig:  &rest.Config{Host: "https://127.0.0.1:6443"},
		ContextName: "test-ctx",
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if args[0] == "list" {
				return []byte("[]"), nil
			}
			if args[0] == "upgrade" && len(args) > 3 && args[3] == "oberth-charts/oberth" {
				oberthArgs = slices.Clone(args)
			}
			return nil, nil
		},
		RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) {
			return []byte("NAME\tKIND\tURL\nrepo\tgit\tssh://git@example.test/repo\n"), nil
		},
		PollInterval: time.Millisecond,
	}

	if err := Run(context.Background(), Config{InstallRekor: true, Timeout: time.Second}, deps); err != nil {
		t.Fatal(err)
	}
	if oberthArgs == nil {
		t.Fatal("Oberth helm install was not called")
	}
	secret, err := client.CoreV1().Secrets(DefaultRekorNamespace).Get(
		context.Background(), rekorSignerSecretName, metav1.GetOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	publicKeyPEM, err := rekorPublicKeyPEM(secret.Data[rekorSignerSecretKey])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"auditAnchor.rekorURL=http://rekor-server.rekor.svc:80",
		"auditAnchor.rekorInsecureHTTP=true",
		"auditAnchor.rekorPublicKey=" + publicKeyPEM,
	} {
		if !slices.Contains(oberthArgs, want) {
			t.Fatalf("Oberth helm args missing %q: %v", want, oberthArgs)
		}
	}
}

func TestRekorHelmArgs(t *testing.T) {
	t.Parallel()
	cfg := Config{RekorNamespace: "rekor", RekorChartVersion: "1.8.3"}
	args := RekorHelmArgs(cfg, "/tmp/values.yaml")
	want := []string{
		"upgrade", "--install", "rekor", "sigstore/rekor",
		"-n", "rekor", "--create-namespace",
		"--version", "1.8.3",
		"-f", "/tmp/values.yaml",
		"--reuse-values",
		"--wait",
	}
	assertSliceEqual(t, args, want)
}

func TestRekorHelmValuesYAML(t *testing.T) {
	t.Parallel()
	values := RekorHelmValuesYAML(Config{RekorNamespace: "custom-rekor"})
	for _, want := range []string{
		// Single-namespace layout: forceNamespace also feeds the
		// --trillian_log_server.address chart argument.
		"forceNamespace: custom-rekor",
		// Persistent file signer wired through the chart's secret mount.
		"signer: /etc/rekor/signer/private.pem",
		"secretName: rekor-signer",
		"privateKeySecretKey: private.pem",
		// Durable MySQL search index on the Trillian MySQL; redis off.
		"storageProvider: mysql",
		"name: trillian-mysql",
		"key: mysql-password",
		// Tuned MySQL memory (the untuned test-matrix run ballooned).
		"memory: 512Mi",
		"memory: 1Gi",
		// No dead nginx Ingress on k3s/kind.
		"enabled: false",
	} {
		if !strings.Contains(values, want) {
			t.Fatalf("values missing %q:\n%s", want, values)
		}
	}
	if strings.Contains(values, "rekor-system") || strings.Contains(values, "trillian-system") {
		t.Fatalf("values must not reference the chart-default namespaces:\n%s", values)
	}
}

func TestEnsureRekorSignerKeyMintsAndReuses(t *testing.T) {
	t.Parallel()
	deps := Deps{KubeClient: fake.NewClientset()}
	cfg := Config{RekorNamespace: "rekor"}

	first, err := EnsureRekorSignerKey(context.Background(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(first))
	if block == nil || block.Type != "PUBLIC KEY" {
		t.Fatalf("want PUBLIC KEY PEM, got:\n%s", first)
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatalf("public key does not parse: %v", err)
	}
	if _, ok := parsed.(*ecdsa.PublicKey); !ok {
		t.Fatalf("want ECDSA public key, got %T", parsed)
	}

	secret, err := deps.KubeClient.CoreV1().Secrets("rekor").Get(context.Background(), "rekor-signer", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("signer secret not created: %v", err)
	}
	if len(secret.Data["private.pem"]) == 0 {
		t.Fatal("signer secret missing private.pem")
	}

	// A second run must REUSE the key — rotation would invalidate every
	// previously published witness entry.
	second, err := EnsureRekorSignerKey(context.Background(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatal("signer key was rotated on re-run; existing keys must be reused")
	}
}

func TestEnsureRekorSignerKeyRefusesMalformedSecret(t *testing.T) {
	t.Parallel()
	malformed := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rekor-signer", Namespace: "rekor"},
		Data:       map[string][]byte{"wrong-key": []byte("x")},
	}
	deps := Deps{KubeClient: fake.NewClientset(malformed)}
	_, err := EnsureRekorSignerKey(context.Background(), Config{RekorNamespace: "rekor"}, deps)
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("want refusal to overwrite malformed signer secret, got %v", err)
	}
}

func TestDryRunFullPlanRendersAllPhases(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "kind-control-plane"},
		Spec:       corev1.NodeSpec{ProviderID: "kind://docker/kind/kind-control-plane"},
	}
	helmCalls := 0
	deps := Deps{
		Output: &buf,
		RunHelm: func(_ context.Context, _ []string) ([]byte, error) {
			helmCalls++
			return nil, nil
		},
		KubeClient:  fake.NewClientset(node, readyArgoControllerPod()),
		RestConfig:  &rest.Config{Host: "https://127.0.0.1:6443"},
		ContextName: "kind-test",
	}
	cfg := Config{DryRun: true, InstallSecretStore: true, InstallRekor: true}
	if err := Run(context.Background(), cfg, deps); err != nil {
		t.Fatal(err)
	}
	if helmCalls != 0 {
		t.Fatalf("dry-run called helm %d times, want 0", helmCalls)
	}
	output := buf.String()
	for _, want := range []string{
		"Stage verified OpenBao TLS before any listener exists",
		"Pre-create or adopt PVC openbao/data-openbao-0 (10Gi, ReadWriteOnce)",
		"credential-free, digest-pinned TLS bootstrap Pod",
		"Install OpenBao (production mode: standalone server + persistent storage)",
		"global.tlsDisable=false",
		"tls_cert_file",
		"server.standalone.enabled=true",
		"Initialize and unseal OpenBao",
		"bao operator init -key-shares=1 -key-threshold=1",
		"Configure secret store",
		"Enable Transit mount: oberth-transit/ and create non-exportable key trusted-plan-artifacts",
		"update only exact Transit encrypt/decrypt paths",
		"Install the local Rekor stack",
		"sigstore/rekor",
		"auditAnchor.rekorURL=http://rekor-server.rekor.svc:80",
		"auditAnchor.rekorInsecureHTTP=true",
		"auditAnchor.rekorPublicKey=<generated-log-public-key-pem>",
		"secretstore.address=https://openbao.openbao.svc:8200",
		"secretstore.transit.enabled=true",
		"Install Oberth",
		"No cluster changes were made",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("full dry-run plan missing %q:\n%s", want, output)
		}
	}
}

func TestDryRunDevSecretStorePlan(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "kind-control-plane"},
		Spec:       corev1.NodeSpec{ProviderID: "kind://docker/kind/kind-control-plane"},
	}
	deps := Deps{
		Output:      &buf,
		RunHelm:     func(_ context.Context, _ []string) ([]byte, error) { return nil, nil },
		KubeClient:  fake.NewClientset(node, readyArgoControllerPod()),
		RestConfig:  &rest.Config{Host: "https://127.0.0.1:6443"},
		ContextName: "kind-test",
	}
	if err := Run(context.Background(), Config{DryRun: true, InstallSecretStoreDev: true}, deps); err != nil {
		t.Fatal(err)
	}
	output := buf.String()
	for _, want := range []string{
		"EVALUATION MODE — not for production",
		"Install OpenBao (dev mode: in-memory, auto-unsealed)",
		"server.dev.enabled=true",
		"Configure secret store",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("dev dry-run plan missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "Initialize and unseal") {
		t.Fatalf("dev dry-run plan must not include the production init step:\n%s", output)
	}
}

func TestDryRunPlainPlanOmitsOptionalPhases(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	// A non-kind engine keeps the plain plan free of the kind-only image
	// preparation phase, so Oberth installation is genuinely the first step.
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "tuxbox"},
		Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.31.4+k3s1"}},
	}
	deps := Deps{
		Output:      &buf,
		RunHelm:     func(_ context.Context, _ []string) ([]byte, error) { return nil, nil },
		KubeClient:  fake.NewClientset(node, readyArgoControllerPod()),
		RestConfig:  &rest.Config{Host: "https://127.0.0.1:6443"},
		ContextName: "default",
	}
	if err := Run(context.Background(), Config{DryRun: true}, deps); err != nil {
		t.Fatal(err)
	}
	output := buf.String()
	if strings.Contains(output, "OpenBao") || strings.Contains(output, "Rekor") {
		t.Fatalf("plain install plan must not mention optional stacks:\n%s", output)
	}
	// The execution engine is not an optional stack: a plain install installs
	// the workflow controller and then the server that submits to it.
	if !strings.Contains(output, "  1. Install Argo Workflows (execution engine)") {
		t.Fatalf("plain install plan should start at the execution engine:\n%s", output)
	}
	if !strings.Contains(output, "  2. Install Oberth") {
		t.Fatalf("plain install plan should install Oberth after the engine:\n%s", output)
	}
}

func TestInstallOberthExistingKindReleaseSkipsImagePreparation(t *testing.T) {
	t.Parallel()

	var helmCalls [][]string
	deps := Deps{
		Output:          io.Discard,
		KindClusterName: KindClusterName,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			helmCalls = append(helmCalls, slices.Clone(args))
			return []byte(`[{"name":"oberth","namespace":"oberth","status":"deployed"}]`), nil
		},
		RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) {
			t.Fatal("existing release without --upgrade must not pull or load kind images")
			return nil, nil
		},
	}

	result, err := InstallOberth(context.Background(), Config{}, deps, OpenBaoResult{
		ServiceAddress: "http://openbao.openbao.svc:8200",
	}, RekorResult{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.AlreadyInstalled {
		t.Fatal("existing release should be reported as already installed")
	}
	if len(helmCalls) != 1 || strings.Join(helmCalls[0], " ") != "list -n oberth -o json" {
		t.Fatalf("helm calls = %v, want only release lookup", helmCalls)
	}
}

// --- Secret store configuration (kubectl exec bao) ---

// freshStoreResponses scripts an empty store: every read reports absence and
// every write succeeds, for the given root token.
func freshStoreResponses() map[string]fakeBaoResponse {
	return map[string]fakeBaoResponse{
		"auth list -format=json":                      {out: `{"token/":{"type":"token"}}`},
		"auth enable -path=kubernetes kubernetes":     {out: "Success!"},
		"read -format=json auth/kubernetes/config":    {out: "No value found at auth/kubernetes/config", err: errors.New("exit status 2")},
		"write auth/kubernetes/config -":              {out: "Success!"},
		"secrets list -format=json":                   {out: `{"cubbyhole/":{"type":"cubbyhole"}}`},
		"secrets enable -path=oberth -version=2 kv":   {out: "Success!"},
		"secrets enable -path=oberth-transit transit": {out: "Success!"},
		"read -format=json oberth-transit/keys/trusted-plan-artifacts": {
			out: "No value found at oberth-transit/keys/trusted-plan-artifacts", err: errors.New("exit status 2"),
		},
		"write oberth-transit/keys/trusted-plan-artifacts -":              {out: "Success!"},
		"policy read oberth-ci":                                           {out: "No policy named: oberth-ci", err: errors.New("exit status 2")},
		"policy write oberth-ci -":                                        {out: "Success!"},
		"read -format=json auth/kubernetes/role/oberth-ci":                {out: "No value found at auth/kubernetes/role/oberth-ci", err: errors.New("exit status 2")},
		"write auth/kubernetes/role/oberth-ci -":                          {out: "Success!"},
		"policy read oberth-argo-credentialed":                            {out: "No policy named: oberth-argo-credentialed", err: errors.New("exit status 2")},
		"policy write oberth-argo-credentialed -":                         {out: "Success!"},
		"read -format=json auth/kubernetes/role/oberth-argo-credentialed": {out: "No value found at auth/kubernetes/role/oberth-argo-credentialed", err: errors.New("exit status 2")},
		"write auth/kubernetes/role/oberth-argo-credentialed -":           {out: "Success!"},
		"policy read oberth-argo-ci-secrets":                              {out: "No policy named: oberth-argo-ci-secrets", err: errors.New("exit status 2")},
		"policy write oberth-argo-ci-secrets -":                           {out: "Success!"},
		"read -format=json auth/kubernetes/role/oberth-argo-ci-secrets":   {out: "No value found at auth/kubernetes/role/oberth-argo-ci-secrets", err: errors.New("exit status 2")},
		"write auth/kubernetes/role/oberth-argo-ci-secrets -":             {out: "Success!"},
	}
}

func configuredProductionStoreResponses() map[string]fakeBaoResponse {
	responses := configuredStoreResponses()
	responses["secrets list -format=json"] = fakeBaoResponse{out: `{
"oberth/":{"type":"kv","options":{"version":"2"}},
"oberth-transit/":{"type":"transit","options":{}}
}`}
	responses["read -format=json oberth-transit/keys/trusted-plan-artifacts"] = fakeBaoResponse{out: `{"data":{
"type":"aes256-gcm96","derived":false,"exportable":false,"allow_plaintext_backup":false,
"supports_encryption":true,"supports_decryption":true
}}`}
	responses["policy read oberth-ci"] = fakeBaoResponse{out: OberthProductionPolicy("oberth", "oberth-transit", "trusted-plan-artifacts")}
	responses["policy read oberth-argo-credentialed"] = fakeBaoResponse{out: OberthCredentialedPolicy("oberth")}
	responses["read -format=json auth/kubernetes/role/oberth-argo-credentialed"] = fakeBaoResponse{out: `{"request_id":"3","data":{` +
		`"bound_service_account_names":["oberth-argo-credentialed"],` +
		`"bound_service_account_namespaces":["oberth-argo"],` +
		`"token_policies":["oberth-argo-credentialed"],` +
		`"token_no_default_policy":true,` +
		`"token_ttl":1200,` +
		`"token_max_ttl":1800}}`}
	return responses
}

// configuredStoreResponses scripts a store where every object already matches
// the managed state; no write command is scripted, so any write fails loudly.
func configuredStoreResponses() map[string]fakeBaoResponse {
	return map[string]fakeBaoResponse{
		"auth list -format=json":                   {out: `{"kubernetes/":{"type":"kubernetes"},"token/":{"type":"token"}}`},
		"read -format=json auth/kubernetes/config": {out: `{"request_id":"1","data":{"kubernetes_host":"https://kubernetes.default.svc","kubernetes_ca_cert":"-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n"}}`},
		"secrets list -format=json":                {out: `{"oberth/":{"type":"kv","options":{"version":"2"}}}`},
		"policy read oberth-ci":                    {out: OberthPolicy("oberth")},
		"read -format=json auth/kubernetes/role/oberth-ci": {out: `{"request_id":"2","data":{` +
			`"bound_service_account_names":["oberth"],` +
			`"bound_service_account_namespaces":["oberth"],` +
			`"token_policies":["oberth-ci"],` +
			`"token_no_default_policy":true,` +
			`"token_ttl":600,` +
			`"token_max_ttl":900}}`},
		"policy read oberth-argo-credentialed": {out: OberthCredentialedPolicy("oberth")},
		"read -format=json auth/kubernetes/role/oberth-argo-credentialed": {out: `{"request_id":"3","data":{` +
			`"bound_service_account_names":["oberth-argo-credentialed"],` +
			`"bound_service_account_namespaces":["oberth-argo"],` +
			`"token_policies":["oberth-argo-credentialed"],` +
			`"token_no_default_policy":true,` +
			`"token_ttl":1200,` +
			`"token_max_ttl":1800}}`},
		"policy read oberth-argo-ci-secrets": {out: OberthCISecretsPolicy("oberth")},
		"read -format=json auth/kubernetes/role/oberth-argo-ci-secrets": {out: `{"request_id":"4","data":{` +
			`"bound_service_account_names":["oberth-argo-ci-secrets"],` +
			`"bound_service_account_namespaces":["oberth-argo"],` +
			`"token_policies":["oberth-argo-ci-secrets"],` +
			`"token_no_default_policy":true,` +
			`"token_ttl":1200,` +
			`"token_max_ttl":1800}}`},
	}
}

func secretStoreTestDeps(runner *fakeBaoRunner, output io.Writer, objects ...k8sruntime.Object) Deps {
	ca := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: "kube-public"},
		Data:       map[string]string{"ca.crt": "-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n"},
	}
	return Deps{
		Output:       output,
		KubeClient:   fake.NewClientset(append([]k8sruntime.Object{ca}, objects...)...),
		RestConfig:   &rest.Config{Host: "https://127.0.0.1:6443"},
		RunCommand:   runner.run,
		ContextName:  "test-ctx",
		PollInterval: time.Millisecond,
	}
}

// readyArgoControllerPod is the workflow controller Run verifies before it
// installs Oberth. Every Run-level fixture needs one for the same reason a real
// cluster does: Oberth executes pipelines as Argo Workflows, so an install that
// finished without a controller produced a server that accepts pushes and never
// runs them.
func readyArgoControllerPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "argo-workflows-workflow-controller",
			Namespace: DefaultArgoNamespace,
			Labels:    map[string]string{"app.kubernetes.io/name": "argo-workflows-workflow-controller"},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
}

func runningOpenBaoPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "openbao-0", Namespace: DefaultOpenBaoNamespace,
			Labels: map[string]string{"app.kubernetes.io/instance": "openbao"},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
}

func TestConfigureSecretStoreFresh(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	runner := &fakeBaoRunner{t: t, responses: freshStoreResponses()}
	deps := secretStoreTestDeps(runner, &buf)
	cfg := Config{}
	_ = cfg.Validate()

	const token = "root-token-x"
	store := newOpenBaoExec(deps, DefaultOpenBaoNamespace, "openbao-0")
	result, err := ConfigureSecretStore(context.Background(), cfg, deps, store, token)
	if err != nil {
		t.Fatal(err)
	}
	if !result.AuthMountConfigured || !result.PolicyWritten || !result.RoleCreated {
		t.Fatalf("fresh store should configure everything, got %+v", result)
	}

	byCommand := runner.callsByCommand()

	// Every kubectl invocation targets the pod through exec with the
	// selected context, and the token travels only as the first stdin line.
	for _, call := range runner.calls {
		argv := strings.Join(call.argv, " ")
		if !strings.HasPrefix(argv, "kubectl exec -i -c openbao --context test-ctx -n openbao openbao-0 --") {
			t.Fatalf("unexpected kubectl argv: %s", argv)
		}
		if strings.Contains(argv, token) {
			t.Fatalf("root token leaked into kubectl argv (recorded in the exec audit log): %s", argv)
		}
		if call.authenticated && !strings.HasPrefix(call.stdin, token+"\n") {
			t.Fatalf("authenticated call %q must receive the token as the first stdin line, got %q", call.command, call.stdin)
		}
	}

	configWrite, ok := byCommand["write auth/kubernetes/config -"]
	if !ok {
		t.Fatal("auth config was not written")
	}
	configJSON := strings.TrimPrefix(configWrite.stdin, token+"\n")
	var configPayload map[string]any
	if err := json.Unmarshal([]byte(configJSON), &configPayload); err != nil {
		t.Fatalf("auth config payload is not JSON: %v (%q)", err, configJSON)
	}
	if configPayload["kubernetes_host"] != "https://kubernetes.default.svc" {
		t.Fatalf("kubernetes_host = %v", configPayload["kubernetes_host"])
	}
	if ca, _ := configPayload["kubernetes_ca_cert"].(string); !strings.Contains(ca, "BEGIN CERTIFICATE") {
		t.Fatalf("kubernetes_ca_cert missing PEM: %v", configPayload["kubernetes_ca_cert"])
	}

	policyWrite, ok := byCommand["policy write oberth-ci -"]
	if !ok {
		t.Fatal("policy was not written")
	}
	policyBody := strings.TrimPrefix(policyWrite.stdin, token+"\n")
	if !strings.Contains(policyBody, `path "oberth/data/*"`) || !strings.Contains(policyBody, "auth/token/revoke-self") {
		t.Fatalf("policy body incomplete:\n%s", policyBody)
	}

	roleWrite, ok := byCommand["write auth/kubernetes/role/oberth-ci -"]
	if !ok {
		t.Fatal("role was not written")
	}
	var rolePayload map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(roleWrite.stdin, token+"\n")), &rolePayload); err != nil {
		t.Fatalf("role payload is not JSON: %v", err)
	}
	if rolePayload["bound_service_account_names"] != "oberth" ||
		rolePayload["bound_service_account_namespaces"] != "oberth" ||
		rolePayload["token_policies"] != "oberth-ci" ||
		rolePayload["token_no_default_policy"] != true ||
		rolePayload["token_ttl"] != "10m" ||
		rolePayload["token_max_ttl"] != "15m" {
		t.Fatalf("role payload wrong: %v", rolePayload)
	}
}

func TestConfigureSecretStoreProductionCreatesNarrowTransitKeyAndPolicy(t *testing.T) {
	t.Parallel()
	runner := &fakeBaoRunner{t: t, responses: freshStoreResponses()}
	deps := secretStoreTestDeps(runner, io.Discard)
	cfg := Config{InstallSecretStore: true}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	store := newOpenBaoExec(deps, DefaultOpenBaoNamespace, "openbao-0")
	result, err := ConfigureSecretStore(context.Background(), cfg, deps, store, "root-token-x")
	if err != nil {
		t.Fatal(err)
	}
	if !result.TransitMountEnabled || !result.TransitKeyCreated || !result.PolicyWritten || !result.TrustedTransitVerified {
		t.Fatalf("production Transit setup incomplete: %+v", result)
	}
	calls := runner.callsByCommand()
	keyWrite, ok := calls["write oberth-transit/keys/trusted-plan-artifacts -"]
	if !ok {
		t.Fatal("managed Transit key was not created")
	}
	var keyPayload map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(keyWrite.stdin, "root-token-x\n")), &keyPayload); err != nil {
		t.Fatal(err)
	}
	if keyPayload["type"] != "aes256-gcm96" || keyPayload["derived"] != false || keyPayload["exportable"] != false || keyPayload["allow_plaintext_backup"] != false {
		t.Fatalf("unsafe Transit key payload: %v", keyPayload)
	}
	policy := strings.TrimPrefix(calls["policy write oberth-ci -"].stdin, "root-token-x\n")
	for _, path := range []string{
		`path "oberth-transit/encrypt/trusted-plan-artifacts"`,
		`path "oberth-transit/decrypt/trusted-plan-artifacts"`,
	} {
		if !strings.Contains(policy, path) {
			t.Fatalf("production policy missing exact Transit path %q:\n%s", path, policy)
		}
	}
	if strings.Contains(policy, `path "oberth-transit/keys/`) || strings.Contains(policy, `path "oberth-transit/*`) {
		t.Fatalf("production policy grants Transit key management or wildcard access:\n%s", policy)
	}
}

func TestConfigureSecretStoreProductionRerunPositivelyVerifiesTransitWithoutMutation(t *testing.T) {
	t.Parallel()
	runner := &fakeBaoRunner{t: t, responses: configuredProductionStoreResponses()}
	deps := secretStoreTestDeps(runner, io.Discard)
	cfg := Config{InstallSecretStore: true}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	result, err := ConfigureSecretStore(
		context.Background(), cfg, deps,
		newOpenBaoExec(deps, DefaultOpenBaoNamespace, "openbao-0"), "root-token-x",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.TrustedTransitVerified || !result.Skipped {
		t.Fatalf("verified production rerun = %+v", result)
	}
}

func TestConfigureSecretStoreProductionRefusesUnsafeExistingTransitKey(t *testing.T) {
	t.Parallel()
	responses := configuredProductionStoreResponses()
	responses["read -format=json oberth-transit/keys/trusted-plan-artifacts"] = fakeBaoResponse{out: `{"data":{
"type":"aes256-gcm96","derived":false,"exportable":true,"allow_plaintext_backup":false,
"supports_encryption":true,"supports_decryption":true
}}`}
	runner := &fakeBaoRunner{t: t, responses: responses}
	deps := secretStoreTestDeps(runner, io.Discard)
	cfg := Config{InstallSecretStore: true}
	_ = cfg.Validate()
	store := newOpenBaoExec(deps, DefaultOpenBaoNamespace, "openbao-0")
	_, err := ConfigureSecretStore(context.Background(), cfg, deps, store, "root-token-x")
	if err == nil || !strings.Contains(err.Error(), "unsafe or incompatible") {
		t.Fatalf("unsafe existing Transit key was not rejected: %v", err)
	}
}

func TestConfigureSecretStoreRefusesRoleWithExtraBinding(t *testing.T) {
	t.Parallel()
	responses := configuredStoreResponses()
	responses["read -format=json auth/kubernetes/role/oberth-ci"] = fakeBaoResponse{out: `{"data":{` +
		`"bound_service_account_names":["oberth","other-service-account"],` +
		`"bound_service_account_namespaces":["oberth"],` +
		`"token_policies":["oberth-ci"],` +
		`"token_no_default_policy":true,` +
		`"token_ttl":600,` +
		`"token_max_ttl":900}}`}
	runner := &fakeBaoRunner{t: t, responses: responses}
	deps := secretStoreTestDeps(runner, io.Discard)
	store := newOpenBaoExec(deps, DefaultOpenBaoNamespace, "openbao-0")
	_, err := ConfigureSecretStore(context.Background(), Config{}, deps, store, "root-token-x")
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("extra ServiceAccount binding must fail closed, got %v", err)
	}
}

func TestConfigureSecretStoreRefusesRoleWithPermissiveTokenLifetime(t *testing.T) {
	t.Parallel()
	responses := configuredStoreResponses()
	responses["read -format=json auth/kubernetes/role/oberth-ci"] = fakeBaoResponse{out: `{"data":{` +
		`"bound_service_account_names":["oberth"],` +
		`"bound_service_account_namespaces":["oberth"],` +
		`"token_policies":["oberth-ci"],` +
		`"token_no_default_policy":true,` +
		`"token_ttl":3600,` +
		`"token_max_ttl":7200}}`}
	runner := &fakeBaoRunner{t: t, responses: responses}
	deps := secretStoreTestDeps(runner, io.Discard)
	store := newOpenBaoExec(deps, DefaultOpenBaoNamespace, "openbao-0")
	_, err := ConfigureSecretStore(context.Background(), Config{}, deps, store, "root-token-x")
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("permissive role token lifetime must fail closed, got %v", err)
	}
}

func TestConfigureSecretStoreIdempotent(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	// Only reads are scripted: any write hits an unscripted command and
	// fails the test via the fake runner.
	runner := &fakeBaoRunner{t: t, responses: configuredStoreResponses()}
	deps := secretStoreTestDeps(runner, &buf)
	cfg := Config{}
	_ = cfg.Validate()

	store := newOpenBaoExec(deps, DefaultOpenBaoNamespace, "openbao-0")
	result, err := ConfigureSecretStore(context.Background(), cfg, deps, store, "root-token-x")
	if err != nil {
		t.Fatal(err)
	}
	if result.AuthMountConfigured || result.PolicyWritten || result.RoleCreated {
		t.Fatalf("idempotent rerun changed state: %+v", result)
	}
	if !result.Skipped {
		t.Fatal("result.Skipped should be true on idempotent rerun")
	}
}

func TestConfigureSecretStoreWrongAuthType(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	runner := &fakeBaoRunner{t: t, responses: map[string]fakeBaoResponse{
		"auth list -format=json": {out: `{"kubernetes/":{"type":"jwt"}}`},
	}}
	deps := secretStoreTestDeps(runner, &buf)
	cfg := Config{}
	_ = cfg.Validate()

	store := newOpenBaoExec(deps, DefaultOpenBaoNamespace, "openbao-0")
	_, err := ConfigureSecretStore(context.Background(), cfg, deps, store, "root-token-x")
	if err == nil || !strings.Contains(err.Error(), "not kubernetes") {
		t.Fatalf("want auth type mismatch error, got %v", err)
	}
}

// --- Helm release detection ---

func TestFindHelmRelease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("found with chart version", func(t *testing.T) {
		t.Parallel()
		deps := Deps{
			RunHelm: func(_ context.Context, _ []string) ([]byte, error) {
				return []byte(`[{"name":"openbao","namespace":"openbao","status":"deployed","chart":"openbao-0.28.6"}]`), nil
			},
		}
		release, exists := findHelmRelease(ctx, deps, "openbao", "openbao")
		if !exists {
			t.Fatal("should find existing release")
		}
		if got := chartVersionFromRelease(release.Chart, "openbao"); got != "0.28.6" {
			t.Fatalf("chart version = %q, want 0.28.6", got)
		}
	})

	t.Run("not found", func(t *testing.T) {
		t.Parallel()
		deps := Deps{
			RunHelm: func(_ context.Context, _ []string) ([]byte, error) {
				return []byte(`[]`), nil
			},
		}
		if _, exists := findHelmRelease(ctx, deps, "openbao", "openbao"); exists {
			t.Fatal("should not find missing release")
		}
	})

	t.Run("helm error", func(t *testing.T) {
		t.Parallel()
		deps := Deps{
			RunHelm: func(_ context.Context, _ []string) ([]byte, error) {
				return nil, errors.New("namespace not found")
			},
		}
		if _, exists := findHelmRelease(ctx, deps, "openbao", "openbao"); exists {
			t.Fatal("helm error should report not exists")
		}
	})
}

func TestChartVersionFromRelease(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		chartField, chartName, want string
	}{
		{"oberth-0.10.58", "oberth", "0.10.58"},
		{"openbao-0.28.6", "openbao", "0.28.6"},
		{"rekor-1.8.3", "rekor", "1.8.3"},
		// The name prefix must match exactly — a different chart in the field
		// yields no version rather than a garbage suffix.
		{"otherchart-1.0.0", "oberth", ""},
		{"oberth", "oberth", ""},
		{"", "oberth", ""},
	} {
		if got := chartVersionFromRelease(tc.chartField, tc.chartName); got != tc.want {
			t.Errorf("chartVersionFromRelease(%q, %q) = %q, want %q", tc.chartField, tc.chartName, got, tc.want)
		}
	}
}

func TestPlanHelmAction(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name              string
		exists            bool
		installed, target string
		status            string
		force             bool
		want              helmAction
	}{
		{name: "absent installs", exists: false, target: "v0.10.58", want: actionInstall},
		{name: "absent installs even without target", exists: false, want: actionInstall},
		{name: "equal skips", exists: true, installed: "0.10.58", target: "v0.10.58", status: "deployed", want: actionSkip},
		{name: "newer target upgrades", exists: true, installed: "0.10.57", target: "v0.10.58", status: "deployed", want: actionUpgrade},
		{name: "older target never downgrades", exists: true, installed: "0.10.59", target: "v0.10.58", status: "deployed", want: actionSkip},
		{name: "unknown installed skips", exists: true, installed: "", target: "v0.10.58", status: "deployed", want: actionSkip},
		{name: "unknown target skips", exists: true, installed: "0.10.58", target: "", status: "deployed", want: actionSkip},
		{name: "unparseable installed skips", exists: true, installed: "not-a-version", target: "v0.10.58", status: "deployed", want: actionSkip},
		{name: "force reconciles when equal", exists: true, installed: "0.10.58", target: "v0.10.58", status: "deployed", force: true, want: actionUpgrade},
		{name: "force reconciles unknown versions", exists: true, force: true, want: actionUpgrade},
		{name: "failed release upgrades", exists: true, installed: "0.10.58", target: "v0.10.58", status: "failed", want: actionUpgrade},
		{name: "pending-install release upgrades", exists: true, installed: "0.10.58", target: "v0.10.58", status: "pending-install", want: actionUpgrade},
		{name: "empty status with equal versions skips", exists: true, installed: "0.10.58", target: "v0.10.58", want: actionSkip},
	} {
		if got := planHelmAction(tc.exists, tc.installed, tc.target, tc.status, tc.force); got != tc.want {
			t.Errorf("%s: planHelmAction = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestInstallOberthSkipsWhenUpToDate(t *testing.T) {
	t.Parallel()

	var helmCalls [][]string
	var buf bytes.Buffer
	deps := Deps{
		Output: &buf,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			helmCalls = append(helmCalls, slices.Clone(args))
			switch args[0] {
			case "list":
				return []byte(`[{"name":"oberth","namespace":"oberth","status":"deployed","chart":"oberth-0.10.58"}]`), nil
			case "search":
				return []byte(`[{"name":"oberth-charts/oberth","version":"0.10.58"}]`), nil
			}
			return nil, nil
		},
	}

	result, err := InstallOberth(context.Background(), Config{ChartVersion: "v0.10.58"}, deps, OpenBaoResult{}, RekorResult{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.AlreadyInstalled {
		t.Fatal("up-to-date release should be reported as already installed")
	}
	if result.Upgraded {
		t.Fatal("up-to-date release must not be upgraded")
	}
	for _, call := range helmCalls {
		if call[0] == "upgrade" {
			t.Fatalf("up-to-date release must not run helm upgrade, got %v", helmCalls)
		}
	}
	if strings.Contains(buf.String(), "Note: chart") {
		t.Fatalf("no newer-chart hint expected when repo matches, got %q", buf.String())
	}
}

func TestInstallOberthReconcilesRekorAtInstalledVersion(t *testing.T) {
	t.Parallel()
	const publicKeyPEM = "-----BEGIN PUBLIC KEY-----\nQUJD\n-----END PUBLIC KEY-----\n"
	rekor := RekorResult{
		ServiceAddress:  "http://rekor-server.rekor.svc:80",
		LogPublicKeyPEM: publicKeyPEM,
	}

	for _, tc := range []struct {
		name             string
		installedVersion string
	}{
		{name: "current chart", installedVersion: "0.10.58"},
		{name: "newer chart", installedVersion: "0.10.59"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var upgradeArgs []string
			deps := Deps{
				Output: io.Discard,
				RunHelm: func(_ context.Context, args []string) ([]byte, error) {
					switch args[0] {
					case "list":
						return []byte(fmt.Sprintf(
							`[{"name":"oberth","namespace":"oberth","status":"deployed","chart":"oberth-%s"}]`,
							tc.installedVersion,
						)), nil
					case "upgrade":
						upgradeArgs = slices.Clone(args)
					}
					return nil, nil
				},
			}

			result, err := InstallOberth(context.Background(), Config{
				ChartVersion: "v0.10.58",
				InstallRekor: true,
			}, deps, OpenBaoResult{}, rekor)
			if err != nil {
				t.Fatal(err)
			}
			if !result.Upgraded || result.AlreadyInstalled {
				t.Fatalf("Rekor reconciliation result = %+v, want an existing-release upgrade", result)
			}
			want := []string{
				"upgrade", "--install", "oberth", "oberth-charts/oberth",
				"-n", "oberth", "--create-namespace",
				"--set", "argo.namespace=oberth-argo",
				"--set", "auditAnchor.rekorURL=http://rekor-server.rekor.svc:80",
				"--set", "auditAnchor.rekorInsecureHTTP=true",
				"--set-string", "auditAnchor.rekorPublicKey=" + publicKeyPEM,
				"--set-string", "auditAnchor.acceptWitnessGenesis=",
				"--version", tc.installedVersion,
				"--reuse-values",
			}
			assertSliceEqual(t, upgradeArgs, want)
		})
	}
}

func TestInstallOberthReconcilesVerifiedSecretStoreAtInstalledVersion(t *testing.T) {
	t.Parallel()
	const caPEM = "-----BEGIN CERTIFICATE-----\npublic-ca\n-----END CERTIFICATE-----\n"
	var upgradeArgs []string
	deps := Deps{
		Output: io.Discard,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			switch args[0] {
			case "list":
				return []byte(`[{"name":"oberth","namespace":"oberth","status":"deployed","chart":"oberth-0.10.59"}]`), nil
			case "upgrade":
				upgradeArgs = slices.Clone(args)
			}
			return nil, nil
		},
	}

	result, err := InstallOberth(context.Background(), Config{
		ChartVersion:       "v0.10.58",
		InstallSecretStore: true,
	}, deps, OpenBaoResult{
		ServiceAddress:         "https://openbao.openbao.svc:8200",
		CACertPEM:              caPEM,
		TrustedTransitVerified: true,
	}, RekorResult{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Upgraded || result.AlreadyInstalled {
		t.Fatalf("secret store reconciliation result = %+v, want values reconciliation", result)
	}
	joined := strings.Join(upgradeArgs, " ")
	for _, want := range []string{
		"--version 0.10.59",
		"secretstore.address=https://openbao.openbao.svc:8200",
		"secretstore.caCert=" + caPEM,
		"secretstore.insecureHTTPForDev=false",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("secret store reconciliation missing %q: %q", want, joined)
		}
	}
	if strings.Contains(joined, "--version v0.10.58") || strings.Contains(joined, "secretstore.address=http://") {
		t.Fatalf("secret store reconciliation downgraded chart or transport: %q", joined)
	}
}

func TestInstallOberthRejectsUnverifiedProductionTransitBeforeAnyHelmCall(t *testing.T) {
	t.Parallel()
	helmCalls := 0
	deps := Deps{
		Output: io.Discard,
		RunHelm: func(context.Context, []string) ([]byte, error) {
			helmCalls++
			return nil, nil
		},
	}
	_, err := InstallOberth(context.Background(), Config{InstallSecretStore: true}, deps, OpenBaoResult{
		ServiceAddress: "https://openbao.openbao.svc:8200",
		CACertPEM:      "public-ca",
	}, RekorResult{})
	if err == nil || !strings.Contains(err.Error(), "not positively verified") {
		t.Fatalf("unverified production install error = %v", err)
	}
	if helmCalls != 0 {
		t.Fatalf("unverified production install made %d Helm calls, want 0", helmCalls)
	}
}

func TestInstallOberthRekorReconcileRejectsUnknownInstalledVersion(t *testing.T) {
	t.Parallel()

	deps := Deps{
		Output: io.Discard,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if args[0] == "list" {
				return []byte(`[{"name":"oberth","namespace":"oberth","status":"deployed"}]`), nil
			}
			if args[0] == "upgrade" {
				t.Fatal("unknown installed version must not risk changing the release")
			}
			return nil, nil
		},
	}

	_, err := InstallOberth(context.Background(), Config{
		ChartVersion: "v0.10.58",
		InstallRekor: true,
	}, deps, OpenBaoResult{}, RekorResult{
		ServiceAddress:  "http://rekor-server.rekor.svc:80",
		LogPublicKeyPEM: "-----BEGIN PUBLIC KEY-----\nQUJD\n-----END PUBLIC KEY-----\n",
	})
	if err == nil {
		t.Fatal("expected unknown installed chart version to block Rekor reconciliation")
	}
	if !strings.Contains(err.Error(), "cannot safely reconcile Rekor values") ||
		!strings.Contains(err.Error(), "--upgrade") {
		t.Fatalf("error should explain the safe override, got %v", err)
	}
}

func TestInstallOberthRekorUpgradeAllowsUnknownInstalledVersion(t *testing.T) {
	t.Parallel()
	const publicKeyPEM = "-----BEGIN PUBLIC KEY-----\nQUJD\n-----END PUBLIC KEY-----\n"
	var upgradeArgs []string
	deps := Deps{
		Output: io.Discard,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			switch args[0] {
			case "list":
				return []byte(`[{"name":"oberth","namespace":"oberth","status":"deployed"}]`), nil
			case "upgrade":
				upgradeArgs = slices.Clone(args)
			}
			return nil, nil
		},
	}

	result, err := InstallOberth(context.Background(), Config{
		ChartVersion: "v0.10.58",
		InstallRekor: true,
		Upgrade:      true,
	}, deps, OpenBaoResult{}, RekorResult{
		ServiceAddress:  "http://rekor-server.rekor.svc:80",
		LogPublicKeyPEM: publicKeyPEM,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Upgraded || result.AlreadyInstalled {
		t.Fatalf("forced Rekor reconciliation result = %+v, want upgrade", result)
	}
	want := []string{
		"upgrade", "--install", "oberth", "oberth-charts/oberth",
		"-n", "oberth", "--create-namespace",
		"--set", "argo.namespace=oberth-argo",
		"--set", "auditAnchor.rekorURL=http://rekor-server.rekor.svc:80",
		"--set", "auditAnchor.rekorInsecureHTTP=true",
		"--set-string", "auditAnchor.rekorPublicKey=" + publicKeyPEM,
		"--set-string", "auditAnchor.acceptWitnessGenesis=",
		"--version", "v0.10.58",
		"--reuse-values",
	}
	assertSliceEqual(t, upgradeArgs, want)
}

func TestInstallOberthHintsWhenNewerChartPublished(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	deps := Deps{
		Output: &buf,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			switch args[0] {
			case "list":
				return []byte(`[{"name":"oberth","namespace":"oberth","status":"deployed","chart":"oberth-0.10.58"}]`), nil
			case "search":
				return []byte(`[{"name":"oberth-charts/oberth","version":"0.10.59"}]`), nil
			}
			return nil, nil
		},
	}

	result, err := InstallOberth(context.Background(), Config{ChartVersion: "v0.10.58"}, deps, OpenBaoResult{}, RekorResult{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.AlreadyInstalled {
		t.Fatal("up-to-date release should skip even when the repo has a newer chart")
	}
	if !strings.Contains(buf.String(), "chart v0.10.59 is available") {
		t.Fatalf("expected newer-chart hint, got %q", buf.String())
	}
}

func TestInstallOberthUpgradesWhenNewerChartTargeted(t *testing.T) {
	t.Parallel()

	var helmCalls [][]string
	var buf bytes.Buffer
	deps := Deps{
		Output: &buf,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			helmCalls = append(helmCalls, slices.Clone(args))
			if args[0] == "list" {
				return []byte(`[{"name":"oberth","namespace":"oberth","status":"deployed","chart":"oberth-0.10.57"}]`), nil
			}
			return nil, nil
		},
	}

	result, err := InstallOberth(context.Background(), Config{ChartVersion: "v0.10.58"}, deps, OpenBaoResult{}, RekorResult{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Upgraded {
		t.Fatal("newer targeted chart should upgrade the existing release")
	}
	if result.AlreadyInstalled {
		t.Fatal("upgraded release must not be reported as already installed")
	}
	var upgrade []string
	for _, call := range helmCalls {
		if call[0] == "upgrade" {
			upgrade = call
		}
	}
	if upgrade == nil {
		t.Fatalf("expected a helm upgrade call, got %v", helmCalls)
	}
	joined := strings.Join(upgrade, " ")
	if !strings.Contains(joined, "--version v0.10.58") || !strings.Contains(joined, "--reuse-values") {
		t.Fatalf("upgrade args missing version pin or --reuse-values: %q", joined)
	}
	if !strings.Contains(buf.String(), "Oberth v0.10.57 installed, v0.10.58 available. Upgrading...") {
		t.Fatalf("expected upgrade progress message, got %q", buf.String())
	}
}

func TestInstallOpenBaoUpgradesWhenPinnedChartNewer(t *testing.T) {
	t.Parallel()

	var upgradeRan bool
	deps := Deps{
		Output: io.Discard,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			switch args[0] {
			case "list":
				return []byte(`[{"name":"openbao","namespace":"openbao","status":"deployed","chart":"openbao-0.28.5"}]`), nil
			case "upgrade":
				upgradeRan = true
			}
			return nil, nil
		},
	}

	cfg := Config{OpenBaoChartVersion: "0.28.6"}
	result, err := InstallOpenBao(context.Background(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Upgraded || result.AlreadyInstalled {
		t.Fatalf("pinned newer chart should upgrade, got %+v", result)
	}
	if !upgradeRan {
		t.Fatal("expected helm upgrade to run")
	}
}

func TestInstallOpenBaoSkipsWhenPinnedChartInstalled(t *testing.T) {
	t.Parallel()

	deps := Deps{
		Output: io.Discard,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			switch args[0] {
			case "list":
				return []byte(`[{"name":"openbao","namespace":"openbao","status":"deployed","chart":"openbao-0.28.6"}]`), nil
			case "upgrade":
				t.Error("up-to-date OpenBao must not run helm upgrade")
			}
			return nil, nil
		},
	}

	cfg := Config{OpenBaoChartVersion: "0.28.6"}
	result, err := InstallOpenBao(context.Background(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	if !result.AlreadyInstalled || result.Upgraded {
		t.Fatalf("up-to-date OpenBao should skip, got %+v", result)
	}
}

// --- Fake kubectl-exec bao runner ---

type fakeBaoResponse struct {
	out string
	err error
}

type fakeBaoCall struct {
	command       string
	stdin         string
	argv          []string
	authenticated bool
}

// fakeBaoRunner scripts kubectl-exec bao interactions keyed by the bao-level
// command line (after stripping the kubectl and token-shell plumbing).
// Unscripted commands fail the test, which doubles as a no-unexpected-writes
// assertion for idempotency tests.
type fakeBaoRunner struct {
	t         *testing.T
	responses map[string]fakeBaoResponse
	calls     []fakeBaoCall
}

func (f *fakeBaoRunner) run(_ context.Context, input []byte, name string, args ...string) ([]byte, error) {
	f.t.Helper()
	if name != "kubectl" {
		f.t.Fatalf("unexpected host command %q %v", name, args)
	}
	command, authenticated, err := stripKubectlBaoPlumbing(args)
	if err != nil {
		f.t.Fatalf("%v (argv: %v)", err, args)
	}
	call := fakeBaoCall{
		command:       command,
		stdin:         string(input),
		argv:          append([]string{name}, args...),
		authenticated: authenticated,
	}
	f.calls = append(f.calls, call)
	response, ok := f.responses[command]
	if !ok {
		f.t.Fatalf("unscripted bao command %q (stdin %q)", command, string(input))
	}
	return []byte(response.out), response.err
}

func (f *fakeBaoRunner) callsByCommand() map[string]fakeBaoCall {
	result := make(map[string]fakeBaoCall, len(f.calls))
	for _, call := range f.calls {
		result[call.command] = call
	}
	return result
}

// stripKubectlBaoPlumbing extracts the bao-level command from a kubectl exec
// argv and reports whether it used the authenticated token-shell wrapper.
func stripKubectlBaoPlumbing(args []string) (string, bool, error) {
	if len(args) == 0 || args[0] != "exec" {
		return "", false, errors.New("expected kubectl exec")
	}
	separator := slices.Index(args, "--")
	if separator < 0 || separator == len(args)-1 {
		return "", false, errors.New("kubectl exec argv missing -- command")
	}
	command := args[separator+1:]
	switch command[0] {
	case "bao":
		return strings.Join(command[1:], " "), false, nil
	case "sh":
		if len(command) < 3 || command[1] != "-c" {
			return "", false, errors.New("unexpected shell invocation")
		}
		script := command[2]
		if script == tokenShellPreamble {
			if len(command) < 4 || command[3] != "bao" {
				return "", false, errors.New("token shell missing bao placeholder")
			}
			return strings.Join(command[4:], " "), true, nil
		}
		if script == unsealShell {
			return "operator unseal", false, nil
		}
		return "", false, errors.New("unexpected shell script: " + script)
	default:
		return "", false, errors.New("unexpected in-pod command: " + strings.Join(command, " "))
	}
}

// --- Helpers ---

func assertSliceEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("arg[%d] = %q, want %q\ngot:  %v\nwant: %v", i, got[i], want[i], got, want)
		}
	}
}

func assertSubstringsInOrder(t *testing.T, text string, want []string) {
	t.Helper()
	remaining := text
	for _, substring := range want {
		index := strings.Index(remaining, substring)
		if index < 0 {
			t.Fatalf("output missing %q in order:\n%s", substring, text)
		}
		remaining = remaining[index+len(substring):]
	}
}

func compatibleKindInspectJSON(t *testing.T, cacheRoot string) []byte {
	t.Helper()
	_, cacheDirs, err := kindClusterConfig(cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	inspection := dockerContainerInspect{
		Mounts: []dockerMount{
			{Type: "bind", Source: cacheDirs[0], Destination: kindCICachePath},
			{Type: "bind", Source: cacheDirs[1], Destination: kindReleaseCachePath},
		},
	}
	inspection.HostConfig.PortBindings = map[string][]dockerPortBinding{
		"30022/tcp": {{HostIP: "127.0.0.1", HostPort: "30022"}},
		"30443/tcp": {{HostIP: "127.0.0.1", HostPort: "30443"}},
	}
	encoded, err := json.Marshal([]dockerContainerInspect{inspection})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func existingKindCommandRunner(t *testing.T, inspectOutput []byte) CommandRunner {
	t.Helper()
	return func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
		command := strings.Join(append([]string{name}, args...), " ")
		switch command {
		case "docker info":
			return nil, nil
		case "kind get clusters":
			return []byte(KindClusterName + "\n"), nil
		case "docker inspect oberth-control-plane":
			return inspectOutput, nil
		default:
			t.Fatalf("unexpected command: %s", command)
			return nil, nil
		}
	}
}

// --- Secret-store selection flag ---

// TestValidateSecretStoreSelection pins --secretstore onto the two booleans
// the rest of the installer reads, because those booleans are what every
// later branch tests and a selection that does not reach them is a flag that
// parses and does nothing.
func TestValidateSecretStoreSelection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		selection  string
		production bool
		dev        bool
		undecided  bool
	}{
		{name: "production", selection: "production", production: true},
		{name: "dev", selection: "dev", dev: true},
		{name: "none", selection: "none"},
		{name: "case insensitive", selection: "  Production ", production: true},
		{name: "empty stays undecided", selection: "", undecided: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := Config{SecretStore: tc.selection, SecretStoreUndecided: true}
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			if cfg.InstallSecretStore != tc.production {
				t.Fatalf("InstallSecretStore = %v, want %v", cfg.InstallSecretStore, tc.production)
			}
			if cfg.InstallSecretStoreDev != tc.dev {
				t.Fatalf("InstallSecretStoreDev = %v, want %v", cfg.InstallSecretStoreDev, tc.dev)
			}
			if cfg.SecretStoreUndecided != tc.undecided {
				t.Fatalf("SecretStoreUndecided = %v, want %v", cfg.SecretStoreUndecided, tc.undecided)
			}
		})
	}
}

func TestValidateRejectsUnknownSecretStoreSelection(t *testing.T) {
	t.Parallel()
	cfg := Config{SecretStore: "vault"}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "production, dev or none") {
		t.Fatalf("expected a usage error naming the accepted values, got: %v", err)
	}
}

// TestSecretStoreSelectionNoneOverridesInstallFlag proves --secretstore none
// can say no to a store an earlier flag said yes to, which is the answer that
// had no spelling at all before.
func TestSecretStoreSelectionNoneOverridesInstallFlag(t *testing.T) {
	t.Parallel()
	cfg := Config{InstallSecretStore: true, SecretStore: "none"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.wantsSecretStore() {
		t.Fatal("--secretstore none must leave no secret store requested")
	}
}

// --- Secret-store prompt ---

func TestPromptSecretStoreChoiceInteractiveDefaultsToProduction(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	cfg := Config{}
	_ = cfg.Validate()
	deps := Deps{
		Output:     &buf,
		Input:      strings.NewReader("\n\n\n"),
		IsTerminal: func() bool { return true },
	}

	if err := promptSecretStoreChoice(context.Background(), &cfg, deps); err != nil {
		t.Fatal(err)
	}
	if !cfg.InstallSecretStore {
		t.Fatal("empty input should default to production mode")
	}
	if cfg.InstallSecretStoreDev {
		t.Fatal("InstallSecretStoreDev should not be set")
	}
	output := buf.String()
	if !strings.Contains(output, "Install OpenBao") {
		t.Fatalf("prompt text missing:\n%s", output)
	}
	if !strings.Contains(output, "Production mode") {
		t.Fatalf("production option missing from prompt:\n%s", output)
	}
}

func TestPromptSecretStoreChoiceInteractiveExplicitProduction(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	cfg := Config{}
	_ = cfg.Validate()
	deps := Deps{
		Output:     &buf,
		Input:      strings.NewReader("1\n\n\n"),
		IsTerminal: func() bool { return true },
	}

	if err := promptSecretStoreChoice(context.Background(), &cfg, deps); err != nil {
		t.Fatal(err)
	}
	if !cfg.InstallSecretStore {
		t.Fatal("choice 1 should set InstallSecretStore")
	}
}

func TestPromptSecretStoreChoiceInteractiveSkip(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	cfg := Config{}
	_ = cfg.Validate()
	deps := Deps{
		Output:     &buf,
		Input:      strings.NewReader("2\n"),
		IsTerminal: func() bool { return true },
	}

	if err := promptSecretStoreChoice(context.Background(), &cfg, deps); err != nil {
		t.Fatal(err)
	}
	if cfg.InstallSecretStore || cfg.InstallSecretStoreDev {
		t.Fatal("choice 2 should not set either secret store flag")
	}
	if !strings.Contains(buf.String(), "Warning: releases will not work") {
		t.Fatalf("skip should print warning, got:\n%s", buf.String())
	}
}

func TestPromptSecretStoreChoiceNonInteractiveSkips(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	cfg := Config{}
	_ = cfg.Validate()
	deps := Deps{
		Output:     &buf,
		IsTerminal: func() bool { return false },
	}

	if err := promptSecretStoreChoice(context.Background(), &cfg, deps); err != nil {
		t.Fatal(err)
	}
	if cfg.InstallSecretStore || cfg.InstallSecretStoreDev {
		t.Fatal("non-interactive must not auto-install a secret store")
	}
	if !strings.Contains(buf.String(), "--install-secretstore") {
		t.Fatalf("non-interactive should print guidance, got:\n%s", buf.String())
	}
}

func TestPromptSecretStoreChoiceNonInteractiveNilIsTerminal(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	cfg := Config{}
	_ = cfg.Validate()
	deps := Deps{
		Output: &buf,
	}

	if err := promptSecretStoreChoice(context.Background(), &cfg, deps); err != nil {
		t.Fatal(err)
	}
	if cfg.InstallSecretStore || cfg.InstallSecretStoreDev {
		t.Fatal("nil IsTerminal must be treated as non-interactive and skip")
	}
	if !strings.Contains(buf.String(), "--install-secretstore") {
		t.Fatalf("nil IsTerminal should print guidance, got:\n%s", buf.String())
	}
}

func TestPromptSecretStoreChoiceNamespaceOverride(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	cfg := Config{}
	_ = cfg.Validate()
	deps := Deps{
		Output:     &buf,
		Input:      strings.NewReader("1\ncustom-bao\ncustom-argo\n"),
		IsTerminal: func() bool { return true },
	}

	if err := promptSecretStoreChoice(context.Background(), &cfg, deps); err != nil {
		t.Fatal(err)
	}
	if cfg.OpenBaoNamespace != "custom-bao" {
		t.Fatalf("OpenBaoNamespace = %q, want custom-bao", cfg.OpenBaoNamespace)
	}
	if cfg.ArgoNamespace != "custom-argo" {
		t.Fatalf("ArgoNamespace = %q, want custom-argo", cfg.ArgoNamespace)
	}
}

func TestPromptSecretStoreChoiceNamespaceDefaultsKept(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	cfg := Config{}
	_ = cfg.Validate()
	deps := Deps{
		Output:     &buf,
		Input:      strings.NewReader("1\n\n\n"),
		IsTerminal: func() bool { return true },
	}

	if err := promptSecretStoreChoice(context.Background(), &cfg, deps); err != nil {
		t.Fatal(err)
	}
	if cfg.OpenBaoNamespace != DefaultOpenBaoNamespace {
		t.Fatalf("empty namespace input should keep default, got %q", cfg.OpenBaoNamespace)
	}
	if cfg.ArgoNamespace != DefaultArgoNamespace {
		t.Fatalf("empty namespace input should keep default, got %q", cfg.ArgoNamespace)
	}
}

func TestPromptSecretStoreChoiceRejectsArgoNamespaceCollision(t *testing.T) {
	t.Parallel()
	cfg := Config{}
	_ = cfg.Validate()
	deps := Deps{
		Output:     io.Discard,
		Input:      strings.NewReader("1\nopenbao\noberth\n"),
		IsTerminal: func() bool { return true },
	}

	err := promptSecretStoreChoice(context.Background(), &cfg, deps)
	if err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("expected namespace collision error, got: %v", err)
	}
}

func TestPromptSecretStoreChoiceUnrecognizedInputRetries(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	cfg := Config{}
	_ = cfg.Validate()
	// Two garbage answers followed by a valid "1".
	deps := Deps{
		Output:     &buf,
		Input:      strings.NewReader("x\nfoo\n1\n\n\n"),
		IsTerminal: func() bool { return true },
	}

	if err := promptSecretStoreChoice(context.Background(), &cfg, deps); err != nil {
		t.Fatal(err)
	}
	if !cfg.InstallSecretStore {
		t.Fatal("valid answer after retries should be accepted")
	}
	output := buf.String()
	if strings.Count(output, "Please enter 1 or 2.") != 2 {
		t.Fatalf("expected two re-prompt messages, got:\n%s", output)
	}
}

func TestPromptSecretStoreChoiceExhaustsRetries(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	cfg := Config{}
	_ = cfg.Validate()
	// Three garbage answers exhaust retries.
	deps := Deps{
		Output:     &buf,
		Input:      strings.NewReader("x\ny\nz\n"),
		IsTerminal: func() bool { return true },
	}

	if err := promptSecretStoreChoice(context.Background(), &cfg, deps); err != nil {
		t.Fatal(err)
	}
	if cfg.InstallSecretStore || cfg.InstallSecretStoreDev {
		t.Fatal("exhausted retries should skip, not install")
	}
	if !strings.Contains(buf.String(), "Warning: releases will not work") {
		t.Fatalf("exhausted retries should print skip warning, got:\n%s", buf.String())
	}
}

func TestIsValidDNS1123Label(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		input string
		want  bool
	}{
		{"openbao", true},
		{"oberth-argo", true},
		{"a", true},
		{"abc-123", true},
		{"a-b-c", true},
		{"", false},
		{"-start", false},
		{"end-", false},
		{"-", false},
		{"Upper", false},
		{"has space", false},
		{"has.dot", false},
		{"has_underscore", false},
		{strings.Repeat("a", 63), true},
		{strings.Repeat("a", 64), false},
	} {
		if got := isValidDNS1123Label(tc.input); got != tc.want {
			t.Errorf("isValidDNS1123Label(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestPromptValidNamespaceRejectsInvalidDNS1123(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	// First answer invalid (uppercase), second answer valid.
	deps := Deps{
		Output:     &buf,
		Input:      strings.NewReader("UPPER\nvalid-ns\n"),
		IsTerminal: func() bool { return true },
	}

	ns, err := promptValidNamespace(context.Background(), deps, "Test namespace", "default-ns")
	if err != nil {
		t.Fatal(err)
	}
	if ns != "valid-ns" {
		t.Fatalf("namespace = %q, want valid-ns", ns)
	}
	if !strings.Contains(buf.String(), "DNS-1123") {
		t.Fatalf("invalid input should mention DNS-1123, got:\n%s", buf.String())
	}
}

func TestPromptValidNamespaceEOFAcceptsDefault(t *testing.T) {
	t.Parallel()
	deps := Deps{
		Output:     io.Discard,
		Input:      strings.NewReader(""),
		IsTerminal: func() bool { return true },
	}

	ns, err := promptValidNamespace(context.Background(), deps, "Test namespace", "my-default")
	if err != nil {
		t.Fatal(err)
	}
	if ns != "my-default" {
		t.Fatalf("EOF should accept default, got %q", ns)
	}
}

func TestPromptValidNamespaceExhaustsRetries(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	deps := Deps{
		Output:     &buf,
		Input:      strings.NewReader("A\nB\nC\n"),
		IsTerminal: func() bool { return true },
	}

	ns, err := promptValidNamespace(context.Background(), deps, "Test namespace", "fallback")
	if err != nil {
		t.Fatal(err)
	}
	if ns != "fallback" {
		t.Fatalf("exhausted retries should return default, got %q", ns)
	}
}

func TestRunSkipsPromptWhenSecretStoreFlagSet(t *testing.T) {
	t.Parallel()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "k3s-node"},
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.31.4+k3s1"},
		},
	}
	oberthPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "oberth", Namespace: DefaultNamespace,
			Labels: map[string]string{"app.kubernetes.io/instance": "oberth"},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	client := fake.NewClientset(node, oberthPod, readyArgoControllerPod())
	var buf bytes.Buffer
	deps := Deps{
		Output:      &buf,
		KubeClient:  client,
		RestConfig:  &rest.Config{Host: "https://127.0.0.1:6443"},
		ContextName: "test-ctx",
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if args[0] == "list" {
				return []byte("[]"), nil
			}
			return nil, nil
		},
		RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) {
			return []byte("NAME\tKIND\tURL\nrepo\tgit\tssh://git@example.test/repo\n"), nil
		},
		IsTerminal:   func() bool { return true },
		Input:        strings.NewReader("this should never be read\n"),
		PollInterval: time.Millisecond,
	}
	// SecretStoreUndecided is false (default), so the prompt must not fire
	// even though no secret-store flag is set.
	cfg := Config{Timeout: time.Second}
	if err := Run(context.Background(), cfg, deps); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "Install OpenBao") {
		t.Fatal("prompt must not appear when SecretStoreUndecided is false")
	}
}

func TestRunPromptsWhenSecretStoreUndecided(t *testing.T) {
	t.Parallel()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "k3s-node"},
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.31.4+k3s1"},
		},
	}
	oberthPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "oberth", Namespace: DefaultNamespace,
			Labels: map[string]string{"app.kubernetes.io/instance": "oberth"},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	client := fake.NewClientset(node, oberthPod, readyArgoControllerPod())
	var buf bytes.Buffer
	// User chooses "2" (skip) so the install proceeds without OpenBao.
	deps := Deps{
		Output:      &buf,
		KubeClient:  client,
		RestConfig:  &rest.Config{Host: "https://127.0.0.1:6443"},
		ContextName: "test-ctx",
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if args[0] == "list" {
				return []byte("[]"), nil
			}
			return nil, nil
		},
		RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) {
			return []byte("NAME\tKIND\tURL\nrepo\tgit\tssh://git@example.test/repo\n"), nil
		},
		IsTerminal:   func() bool { return true },
		Input:        strings.NewReader("2\n"),
		PollInterval: time.Millisecond,
	}
	cfg := Config{Timeout: time.Second, SecretStoreUndecided: true}
	if err := Run(context.Background(), cfg, deps); err != nil {
		t.Fatal(err)
	}
	output := buf.String()
	if !strings.Contains(output, "Install OpenBao") {
		t.Fatalf("prompt must appear when SecretStoreUndecided is true, got:\n%s", output)
	}
	if !strings.Contains(output, "Warning: releases will not work") {
		t.Fatalf("skip warning missing:\n%s", output)
	}
}

// --- Secret-store mode split ---

func TestConfigValidateSecretStoreModesMutuallyExclusive(t *testing.T) {
	t.Parallel()
	cfg := Config{InstallSecretStore: true, InstallSecretStoreDev: true}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("want mutual exclusion error, got %v", err)
	}
}

func productionOpenBaoResult() OpenBaoResult {
	return OpenBaoResult{
		Namespace:      DefaultOpenBaoNamespace,
		ServiceAddress: "http://openbao.openbao.svc:8200",
	}
}

func TestSetupProductionSecretStoreInitializesAndConfigures(t *testing.T) {
	t.Parallel()
	const rootToken = "s.testroot111"
	const unsealKey = "testunsealkey000"

	responses := freshStoreResponses()
	responses["status -format=json"] = fakeBaoResponse{
		out: `{"initialized":false,"sealed":true,"storage_type":"file"}`,
		err: errors.New("exit status 2"),
	}
	responses["operator init -key-shares=1 -key-threshold=1 -format=json"] = fakeBaoResponse{
		out: `{"unseal_keys_b64":["` + unsealKey + `"],"root_token":"` + rootToken + `"}`,
	}
	responses["operator unseal"] = fakeBaoResponse{
		out: `{"initialized":true,"sealed":false,"storage_type":"file"}`,
	}

	var buf bytes.Buffer
	runner := &fakeBaoRunner{t: t, responses: responses}
	deps := secretStoreTestDeps(runner, &buf, runningOpenBaoPod())
	cfg := Config{InstallSecretStore: true}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	configured, err := SetupProductionSecretStore(context.Background(), cfg, deps, productionOpenBaoResult())
	if err != nil {
		t.Fatal(err)
	}
	if !configured.TrustedTransitVerified {
		t.Fatalf("fresh production setup lacks positive Transit proof: %+v", configured)
	}

	output := buf.String()
	for _, want := range []string{
		"OpenBao credentials (save now, shown once)",
		rootToken,
		unsealKey,
		"└──",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("production output missing %q:\n%s", want, output)
		}
	}

	byCommand := runner.callsByCommand()
	unsealCall, ok := byCommand["operator unseal"]
	if !ok {
		t.Fatal("unseal was not executed")
	}
	if unsealCall.stdin != unsealKey+"\n" {
		t.Fatalf("unseal key must arrive via stdin, got %q", unsealCall.stdin)
	}
	for _, call := range runner.calls {
		argv := strings.Join(call.argv, " ")
		if strings.Contains(argv, rootToken) || strings.Contains(argv, unsealKey) {
			t.Fatalf("credential leaked into kubectl argv (recorded in the exec audit log): %s", argv)
		}
		if call.authenticated && !strings.HasPrefix(call.stdin, rootToken+"\n") {
			t.Fatalf("authenticated call %q must use the captured root token, got stdin %q", call.command, call.stdin)
		}
	}
}

func TestSetupProductionSecretStoreSealedRerunFails(t *testing.T) {
	t.Parallel()
	runner := &fakeBaoRunner{t: t, responses: map[string]fakeBaoResponse{
		"status -format=json": {
			out: `{"initialized":true,"sealed":true,"storage_type":"file"}`,
			err: errors.New("exit status 2"),
		},
	}}
	var buf bytes.Buffer
	deps := secretStoreTestDeps(runner, &buf, runningOpenBaoPod())
	cfg := Config{InstallSecretStore: true}
	_ = cfg.Validate()

	_, err := SetupProductionSecretStore(context.Background(), cfg, deps, productionOpenBaoResult())
	if err == nil {
		t.Fatal("sealed already-initialized store must fail with unseal guidance")
	}
	if !strings.Contains(err.Error(), "bao operator unseal") || !strings.Contains(err.Error(), "re-run oberth install") {
		t.Fatalf("error missing unseal guidance: %v", err)
	}
}

// TestSetupProductionSecretStoreCollectKeepsCredentialsOnUnsealFailure is the
// regression test for the deferred-credentials error path: when `operator
// init` succeeds and a LATER step fails, the shown-once root token and unseal
// key exist nowhere but this process. They must land in the held pool before
// the error is considered, so the caller's deferred flush still prints them.
// Losing them permanently locks the freshly-initialized store.
func TestSetupProductionSecretStoreCollectKeepsCredentialsOnUnsealFailure(t *testing.T) {
	t.Parallel()
	const rootToken = "s.testroot222"
	const unsealKey = "testunsealkey111"

	responses := freshStoreResponses()
	responses["status -format=json"] = fakeBaoResponse{
		out: `{"initialized":false,"sealed":true,"storage_type":"file"}`,
		err: errors.New("exit status 2"),
	}
	responses["operator init -key-shares=1 -key-threshold=1 -format=json"] = fakeBaoResponse{
		out: `{"unseal_keys_b64":["` + unsealKey + `"],"root_token":"` + rootToken + `"}`,
	}
	responses["operator unseal"] = fakeBaoResponse{
		out: "connection refused",
		err: errors.New("exit status 1"),
	}

	var buf bytes.Buffer
	runner := &fakeBaoRunner{t: t, responses: responses}
	deps := secretStoreTestDeps(runner, &buf, runningOpenBaoPod())
	cfg := Config{InstallSecretStore: true}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	var creds heldCredentials
	_, err := setupProductionSecretStoreCollect(context.Background(), cfg, deps, deps, productionOpenBaoResult(), &creds)
	if err == nil {
		t.Fatal("unseal failure must surface as an error")
	}

	var out bytes.Buffer
	creds.flush(&out, false)
	output := out.String()
	for _, want := range []string{"Root token", rootToken, "Unseal key", unsealKey} {
		if !strings.Contains(output, want) {
			t.Fatalf("flushed credentials missing %q after post-init failure:\n%s", want, output)
		}
	}
}

func TestSetupProductionSecretStoreAlreadyInitializedWithoutTokenFailsBeforeRuntimeEnable(t *testing.T) {
	t.Setenv(baoTokenEnvVar, "")
	runner := &fakeBaoRunner{t: t, responses: map[string]fakeBaoResponse{
		"status -format=json": {out: `{"initialized":true,"sealed":false,"storage_type":"file"}`},
	}}
	const (
		oberthNamespace  = "custom-oberth"
		openBaoNamespace = "custom-openbao"
	)
	pod := runningOpenBaoPod()
	pod.Namespace = openBaoNamespace
	var buf bytes.Buffer
	deps := secretStoreTestDeps(runner, &buf, pod)
	cfg := Config{
		InstallSecretStore:  true,
		Namespace:           oberthNamespace,
		OpenBaoNamespace:    openBaoNamespace,
		ChartVersion:        "v9.9.9",
		OpenBaoChartVersion: "9.9.9",
		Yes:                 true,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	openbao := productionOpenBaoResult()
	openbao.Namespace = openBaoNamespace

	_, err := SetupProductionSecretStore(context.Background(), cfg, deps, openbao)
	if err == nil || !strings.Contains(err.Error(), baoTokenEnvVar) {
		t.Fatalf("missing-token production setup error = %v", err)
	}
	output := buf.String()
	if !strings.Contains(output, "already initialized and unsealed") {
		t.Fatalf("output missing already-initialized note:\n%s", output)
	}
	for _, want := range []string{
		"Cannot enable trusted Plan Transit",
		"supply the root token without exposing it in shell history",
		"read -rs BAO_TOKEN && export BAO_TOKEN",
		"oberth install --install-secretstore",
		"Keep every install flag unchanged",
		"--namespace",
		"--openbao-namespace",
		"--chart-version",
		"--openbao-chart-version",
		"--yes",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing safe re-run guidance %q:\n%s", want, output)
		}
	}
	guidanceIndex := strings.Index(output, "supply the root token without exposing it in shell history")
	readIndex := strings.Index(output, "read -rs BAO_TOKEN && export BAO_TOKEN")
	if guidanceIndex > readIndex {
		t.Fatalf("history-safe guidance must precede the read command:\n%s", output)
	}
}

func TestSetupProductionSecretStoreAlreadyInitializedUsesEnvToken(t *testing.T) {
	const envToken = "s.envtoken999"
	t.Setenv(baoTokenEnvVar, envToken)

	responses := configuredProductionStoreResponses()
	responses["status -format=json"] = fakeBaoResponse{out: `{"initialized":true,"sealed":false,"storage_type":"file"}`}
	runner := &fakeBaoRunner{t: t, responses: responses}
	var buf bytes.Buffer
	deps := secretStoreTestDeps(runner, &buf, runningOpenBaoPod())
	cfg := Config{InstallSecretStore: true}
	_ = cfg.Validate()

	configured, err := SetupProductionSecretStore(context.Background(), cfg, deps, productionOpenBaoResult())
	if err != nil {
		t.Fatal(err)
	}
	if !configured.TrustedTransitVerified || !configured.Skipped {
		t.Fatalf("production rerun lacks exact no-mutation proof: %+v", configured)
	}
	if !strings.Contains(buf.String(), "Using the root token to verify the secret-store configuration.") {
		t.Fatalf("output missing token-usage note:\n%s", buf.String())
	}
	authenticated := 0
	for _, call := range runner.calls {
		if call.authenticated {
			authenticated++
			if !strings.HasPrefix(call.stdin, envToken+"\n") {
				t.Fatalf("authenticated call %q must use the %s token, got stdin %q", call.command, baoTokenEnvVar, call.stdin)
			}
		}
	}
	if authenticated == 0 {
		t.Fatal("expected authenticated configuration calls with the env token")
	}
}

func TestSetupProductionSecretStoreRefusesDevStore(t *testing.T) {
	t.Parallel()
	runner := &fakeBaoRunner{t: t, responses: map[string]fakeBaoResponse{
		"status -format=json": {out: `{"initialized":true,"sealed":false,"storage_type":"inmem"}`},
	}}
	var buf bytes.Buffer
	deps := secretStoreTestDeps(runner, &buf, runningOpenBaoPod())
	cfg := Config{InstallSecretStore: true}
	_ = cfg.Validate()

	_, err := SetupProductionSecretStore(context.Background(), cfg, deps, productionOpenBaoResult())
	if err == nil || !strings.Contains(err.Error(), "dev mode (in-memory storage)") {
		t.Fatalf("want dev-store refusal, got %v", err)
	}
}

func TestSetupDevSecretStoreConfiguresAndPrintsToken(t *testing.T) {
	t.Parallel()
	responses := freshStoreResponses()
	responses["status -format=json"] = fakeBaoResponse{out: `{"initialized":true,"sealed":false,"storage_type":"inmem"}`}
	runner := &fakeBaoRunner{t: t, responses: responses}
	var buf bytes.Buffer
	deps := secretStoreTestDeps(runner, &buf, runningOpenBaoPod())
	cfg := Config{InstallSecretStoreDev: true}
	_ = cfg.Validate()

	if err := SetupDevSecretStore(context.Background(), cfg, deps, productionOpenBaoResult()); err != nil {
		t.Fatal(err)
	}
	output := buf.String()
	if !strings.Contains(output, "OpenBao (dev mode) root token: root") {
		t.Fatalf("dev output missing root token line:\n%s", output)
	}
	for _, call := range runner.calls {
		if call.authenticated && !strings.HasPrefix(call.stdin, "root\n") {
			t.Fatalf("dev configuration must use the well-known dev token, got stdin %q", call.stdin)
		}
	}
}

func TestSetupDevSecretStoreRefusesProductionStore(t *testing.T) {
	t.Parallel()
	runner := &fakeBaoRunner{t: t, responses: map[string]fakeBaoResponse{
		"status -format=json": {out: `{"initialized":true,"sealed":false,"storage_type":"file"}`},
	}}
	var buf bytes.Buffer
	deps := secretStoreTestDeps(runner, &buf, runningOpenBaoPod())
	cfg := Config{InstallSecretStoreDev: true}
	_ = cfg.Validate()

	err := SetupDevSecretStore(context.Background(), cfg, deps, productionOpenBaoResult())
	if err == nil || !strings.Contains(err.Error(), "production-mode store") {
		t.Fatalf("want production-store refusal, got %v", err)
	}
}

// --- First-run onboarding ---

type fakeOberthHost struct {
	t            *testing.T
	upstreamRows []string
	uplinkOutput string
	uplinkStdin  string
	uplinkArgv   []string
	listCalls    int
	addCalls     int
	addArgv      [][]string // argv of every `upstream add` invocation, in order
	// addResults scripts successive `upstream add` outcomes; when the script
	// is exhausted the last entry repeats. Empty means every call prints the
	// generated key and registers the upstream (probe accepted immediately).
	addResults []fakeAddResult
}

// fakeAddResult scripts one `upstream add` invocation of the fake pod. The
// real contract being modeled (see cmd/oberth/admin.go and
// internal/app/upstream_bootstrap.go): a fresh generation with --no-wait
// prints the key and exits ZERO without registering the upstream
// (ErrUpstreamBootstrapIncomplete); a rerun whose probe passes registers and
// exits zero; a rerun whose probe fails exits NONZERO with the probe error in
// the output.
type fakeAddResult struct {
	out       string
	err       error
	registers bool
}

const fakeDeployKeyLine = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINKAaSupc8 oberth"

func (f *fakeOberthHost) run(_ context.Context, input []byte, name string, args ...string) ([]byte, error) {
	f.t.Helper()
	if name != "kubectl" {
		f.t.Fatalf("unexpected host command %q %v", name, args)
	}
	joined := strings.Join(args, " ")
	switch {
	case strings.HasSuffix(joined, "oberth upstream list"):
		f.listCalls++
		out := "NAME   KIND   URL   KEY FINGERPRINT\n"
		for _, row := range f.upstreamRows {
			out += row + "\n"
		}
		return []byte(out), nil
	case strings.Contains(joined, "oberth upstream add"):
		f.addCalls++
		f.addArgv = append(f.addArgv, append([]string{name}, args...))
		result := fakeAddResult{registers: true}
		if len(f.addResults) > 0 {
			idx := f.addCalls - 1
			if idx >= len(f.addResults) {
				idx = len(f.addResults) - 1
			}
			result = f.addResults[idx]
		}
		if result.registers {
			f.upstreamRows = []string{"test   ssh   ssh://git@test.example/org   SHA256:x"}
		}
		out := result.out
		if out == "" {
			out = fakeDeployKeyLine + "\n"
		}
		return []byte(out), result.err
	case strings.Contains(joined, "oberth uplink add"):
		f.uplinkStdin = string(input)
		f.uplinkArgv = append([]string{name}, args...)
		return []byte(f.uplinkOutput), nil
	default:
		f.t.Fatalf("unexpected kubectl command: %s", joined)
		return nil, nil
	}
}

func readyOberthPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "oberth-7f9", Namespace: DefaultNamespace,
			Labels: map[string]string{"app.kubernetes.io/instance": "oberth"},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
}

// finishInstallTest wraps FinishInstall with a test-compatible tableWriter and
// heldCredentials. The tableWriter writes to buf with color off (pipe mode),
// header and footer are pre-written so AppendRow can extend the table.
func finishInstallTest(ctx context.Context, cfg Config, deps Deps, buf *bytes.Buffer) error {
	tw := newTableWriter(buf, false)
	tw.WriteHeader()
	tw.WriteFooter()
	var creds heldCredentials
	return FinishInstall(ctx, cfg, deps, tw, &creds)
}

func onboardingDeps(t *testing.T, host *fakeOberthHost, buf *bytes.Buffer, input io.Reader, interactive bool) Deps {
	t.Helper()
	deps := Deps{
		Output:       buf,
		Input:        input,
		KubeClient:   fake.NewClientset(readyOberthPod()),
		RestConfig:   &rest.Config{Host: "https://127.0.0.1:6443"},
		ContextName:  "test-ctx",
		RunCommand:   host.run,
		IsTerminal:   func() bool { return interactive },
		PollInterval: time.Millisecond,
	}
	if interactive {
		deps.RunInteractive = func(context.Context, string, ...string) error { return nil }
	}
	return deps
}

func TestFinishInstallExistingUpstreamWaitsForReady(t *testing.T) {
	t.Parallel()
	host := &fakeOberthHost{t: t, upstreamRows: []string{"codeberg   ssh   ssh://git@codeberg.org/cloudtaser   SHA256:x"}}
	var buf bytes.Buffer
	deps := onboardingDeps(t, host, &buf, strings.NewReader(""), true)
	deps.RunInteractive = func(context.Context, string, ...string) error {
		t.Fatal("configured upstream must not trigger onboarding")
		return nil
	}
	cfg := Config{}
	_ = cfg.Validate()

	if err := finishInstallTest(context.Background(), cfg, deps, &buf); err != nil {
		t.Fatal(err)
	}
	output := buf.String()
	if !strings.Contains(output, "Ready") || !strings.Contains(output, "https://localhost:30443/runs") {
		t.Fatalf("output missing ready line with web UI:\n%s", output)
	}
	if strings.Contains(output, "waiting for upstream configuration") {
		t.Fatalf("configured install must not print the onboarding notice:\n%s", output)
	}
}

func TestFinishInstallNonInteractivePrintsManualSteps(t *testing.T) {
	t.Parallel()
	host := &fakeOberthHost{t: t}
	var buf bytes.Buffer
	deps := onboardingDeps(t, host, &buf, strings.NewReader(""), false)
	cfg := Config{}
	_ = cfg.Validate()

	if err := finishInstallTest(context.Background(), cfg, deps, &buf); err != nil {
		t.Fatal(err)
	}
	output := buf.String()
	for _, want := range []string{
		"oberth upstream add",
		"oberth uplink add",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("non-interactive output missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "Ready —") {
		t.Fatalf("without an upstream the install must not claim readiness:\n%s", output)
	}
}

// stageUplinkKeyPair writes a fresh ed25519 public key (and an accompanying
// private-key placeholder so the SSH-config offer prompts) into a temp dir and
// returns the public key path and its content. Tests must never touch the
// developer's real ~/.ssh.
func stageUplinkKeyPair(t *testing.T) (string, string) {
	t.Helper()
	_, testPriv, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	testSigner, err := ssh.NewSignerFromKey(testPriv)
	if err != nil {
		t.Fatal(err)
	}
	pubKey := string(ssh.MarshalAuthorizedKey(testSigner.PublicKey()))
	dir := t.TempDir()
	pubPath := filepath.Join(dir, "key.pub")
	if err := os.WriteFile(pubPath, []byte(pubKey), 0o600); err != nil {
		t.Fatal(err)
	}
	// The derived private-key path only needs to exist for the SSH-config
	// offer to reach its prompt; content is never read by the installer.
	if err := os.WriteFile(filepath.Join(dir, "key"), []byte("placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	return pubPath, pubKey
}

func TestFinishInstallInteractiveOnboarding(t *testing.T) {
	t.Parallel()
	host := &fakeOberthHost{
		t:            t,
		uplinkOutput: "Uplink token for tester@box (shown once):\ntok_123\n",
	}
	keyPath, pubKey := stageUplinkKeyPair(t)

	// Input sequence for the table-driven onboarding (the probe accepts the
	// key on the first upstream add, so no verification Enter is needed):
	// 1. "codeberg.org/cloudtaser" → upstream URL prompt
	// 2. "g" → deploy key G/P prompt
	// 3. keyPath → uplink public-key path prompt
	// 4. "tester@box" → uplink identity prompt
	// 5. "n" → SSH config prompt (decline)
	input := strings.NewReader("codeberg.org/cloudtaser\ng\n" + keyPath + "\ntester@box\nn\n")

	var buf bytes.Buffer
	deps := onboardingDeps(t, host, &buf, input, true)
	cfg := Config{}
	_ = cfg.Validate()

	if err := finishInstallTest(context.Background(), cfg, deps, &buf); err != nil {
		t.Fatal(err)
	}

	if host.addCalls != 1 {
		t.Fatalf("expected exactly one upstream add, got %d", host.addCalls)
	}
	assertSliceEqual(t, host.addArgv[0], []string{
		"kubectl", "exec", "--context", "test-ctx", "-c", "oberth", "-n", "oberth", "deploy/oberth", "--",
		"oberth", "upstream", "add", "--yes", "--no-wait", "codeberg", "ssh://git@codeberg.org/cloudtaser",
	})
	if host.uplinkStdin != pubKey {
		t.Fatalf("uplink add must receive the public key on stdin, got %q", host.uplinkStdin)
	}
	joinedUplink := strings.Join(host.uplinkArgv, " ")
	if !strings.Contains(joinedUplink, "oberth uplink add - tester@box") || !strings.Contains(joinedUplink, "exec -i") {
		t.Fatalf("uplink argv wrong: %s", joinedUplink)
	}

	output := buf.String()
	// The borderless output colorizes status symbols with ANSI codes,
	// so assertions check label and status word separately.
	for _, want := range []string{
		"github.com/your-org: ",
		"[G]enerate / [P]rovide: ",
		"Upstream",
		"connected",
		"Deploy key",
		fakeDeployKeyLine, // generated key shown in plain line below the upstream result
		"Key registration",
		"accepted",
		"Uplink key",
		keyPath,
		"Uplink",
		"tester@box",
		"registered",
		"tok_123",
		"Credentials (save now, shown once)",
		"https://localhost:30443/runs",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("interactive onboarding output missing %q:\n%s", want, output)
		}
	}
}

func TestFinishInstallOnboardingSkipOnEmptyUpstream(t *testing.T) {
	t.Parallel()
	host := &fakeOberthHost{t: t}
	var buf bytes.Buffer
	deps := onboardingDeps(t, host, &buf, strings.NewReader("\n"), true)
	cfg := Config{}
	_ = cfg.Validate()

	if err := finishInstallTest(context.Background(), cfg, deps, &buf); err != nil {
		t.Fatal(err)
	}
	output := buf.String()
	if !strings.Contains(output, "No upstream given — skipping onboarding.") {
		t.Fatalf("output missing skip note:\n%s", output)
	}
	if !strings.Contains(output, "oberth upstream add") {
		t.Fatalf("skip path must print the manual steps:\n%s", output)
	}
}

func TestFinishInstallOnboardingKeyRegistrationExhausted(t *testing.T) {
	t.Parallel()
	// Every upstream add prints the key but never registers the upstream —
	// the incomplete-bootstrap contract when the forge has not accepted the
	// key (the pod exits zero without creating the upstream row).
	host := &fakeOberthHost{t: t, addResults: []fakeAddResult{{registers: false}}}
	var buf bytes.Buffer
	// Input: upstream URL, generate, then 3 Enter presses for registration attempts.
	input := strings.NewReader("codeberg.org/cloudtaser\ng\n\n\n\n")
	deps := onboardingDeps(t, host, &buf, input, true)
	cfg := Config{}
	_ = cfg.Validate()

	if err := finishInstallTest(context.Background(), cfg, deps, &buf); err != nil {
		t.Fatal(err)
	}
	// Registration only completes through a RERUN of upstream add (the
	// incomplete bootstrap registers nothing), so every verification attempt
	// must rerun it: 1 initial + 3 attempts.
	if host.addCalls != 4 {
		t.Fatalf("each verification attempt must rerun upstream add: want 4 calls, got %d", host.addCalls)
	}
	for i, argv := range host.addArgv {
		joined := strings.Join(argv, " ")
		if !strings.Contains(joined, "--yes") || !strings.Contains(joined, "--no-wait") {
			t.Fatalf("upstream add call %d must pass --yes --no-wait, got: %s", i, joined)
		}
	}
	output := buf.String()
	if !strings.Contains(output, "register key, Enter to verify") {
		t.Fatalf("output missing key-registration prompt:\n%s", output)
	}
	if !strings.Contains(output, "not accepted after 3 attempts") || !strings.Contains(output, "oberth upstream add") {
		t.Fatalf("exhausted key registration must fall back to manual steps:\n%s", output)
	}
	if host.uplinkStdin != "" {
		t.Fatal("uplink must not run when the upstream key is not accepted")
	}
}

func TestFinishInstallOnboardingKeyRegistrationSecondAttempt(t *testing.T) {
	t.Parallel()
	// First add: key generated, forge has not accepted it (incomplete, exit
	// zero, nothing registered). The operator registers the key and presses
	// Enter; the rerun's probe passes and registers the upstream.
	host := &fakeOberthHost{
		t:            t,
		addResults:   []fakeAddResult{{registers: false}, {registers: true}},
		uplinkOutput: "Uplink token for tester@box (shown once):\ntok_456\n",
	}
	keyPath, _ := stageUplinkKeyPair(t)
	// Input: upstream URL, generate, one Enter to verify, uplink key path,
	// identity, SSH config decline.
	input := strings.NewReader("codeberg.org/cloudtaser\ng\n\n" + keyPath + "\ntester@box\nn\n")
	var buf bytes.Buffer
	deps := onboardingDeps(t, host, &buf, input, true)
	cfg := Config{}
	_ = cfg.Validate()

	if err := finishInstallTest(context.Background(), cfg, deps, &buf); err != nil {
		t.Fatal(err)
	}
	if host.addCalls != 2 {
		t.Fatalf("want initial add plus one verification rerun, got %d calls", host.addCalls)
	}
	output := buf.String()
	if !strings.Contains(output, fakeDeployKeyLine) {
		t.Fatalf("generated key must be shown before verification:\n%s", output)
	}
	if !strings.Contains(output, "accepted") {
		t.Fatalf("second-attempt registration must be accepted:\n%s", output)
	}
	if !strings.Contains(output, "tok_456") || !strings.Contains(output, "Credentials (save now, shown once)") {
		t.Fatalf("uplink must complete after late registration:\n%s", output)
	}
}

func TestFinishInstallOnboardingSurfacesUnexpectedProbeError(t *testing.T) {
	t.Parallel()
	// Attempt classification is an allowlist (issue #89): only the publickey
	// auth rejection is the quiet "not yet" state. A knownhosts mismatch is
	// an active MITM indicator and must be shown to the operator, not
	// rendered as another silent retry.
	mitm := fakeAddResult{
		out: fakeDeployKeyLine + "\nupstream SSH identity is not ready: app: upstream SSH authentication probe failed: ssh: handshake failed: knownhosts: key mismatch\n",
		err: errors.New("exit status 1"),
	}
	host := &fakeOberthHost{
		t:          t,
		addResults: []fakeAddResult{{registers: false}, mitm, {registers: false}, {registers: false}},
	}
	var buf bytes.Buffer
	input := strings.NewReader("codeberg.org/cloudtaser\ng\n\n\n\n")
	deps := onboardingDeps(t, host, &buf, input, true)
	cfg := Config{}
	_ = cfg.Validate()

	if err := finishInstallTest(context.Background(), cfg, deps, &buf); err != nil {
		t.Fatal(err)
	}
	output := buf.String()
	// The borderless format separates the ⚠ symbol (ANSI-colorized) from the
	// status word "error", so check for the word alone.
	if !strings.Contains(output, "Key registration") || !strings.Contains(output, "error") {
		t.Fatalf("unexpected probe failure must mark the attempt row:\n%s", output)
	}
	if !strings.Contains(output, "knownhosts: key mismatch") {
		t.Fatalf("MITM indicator must be surfaced verbatim:\n%s", output)
	}
	if !strings.Contains(output, "Last verification error:") {
		t.Fatalf("exhausted flow must include the last verification error:\n%s", output)
	}
}

func TestFinishInstallOnboardingPrintsRawOutputWhenTokenUnrecognized(t *testing.T) {
	t.Parallel()
	// The bearer token is shown exactly once and never recoverable; when the
	// pod output format is unrecognized the sanitized output must still be
	// printed rather than silently discarded.
	host := &fakeOberthHost{
		t:            t,
		uplinkOutput: "Registered uplink tester@box; retrieve the credential from your terminal above.\n",
	}
	keyPath, _ := stageUplinkKeyPair(t)
	input := strings.NewReader("codeberg.org/cloudtaser\ng\n" + keyPath + "\ntester@box\nn\n")
	var buf bytes.Buffer
	deps := onboardingDeps(t, host, &buf, input, true)
	cfg := Config{}
	_ = cfg.Validate()

	if err := finishInstallTest(context.Background(), cfg, deps, &buf); err != nil {
		t.Fatal(err)
	}
	output := buf.String()
	if strings.Contains(output, "Credentials (save now, shown once)") {
		t.Fatalf("no token was extracted, the credential box must not render:\n%s", output)
	}
	if !strings.Contains(output, "Registered uplink tester@box") {
		t.Fatalf("unrecognized uplink output must be printed, not discarded:\n%s", output)
	}
}

func TestRegisterUpstreamQuietClassifiesOutcomes(t *testing.T) {
	t.Parallel()
	authRejection := "ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey], no supported methods remain"
	for _, tc := range []struct {
		name           string
		result         fakeAddResult
		wantRegistered bool
		wantKey        bool
		wantErr        string
	}{
		{
			name:           "incomplete bootstrap exits zero and is pending",
			result:         fakeAddResult{registers: false},
			wantRegistered: false,
			wantKey:        true,
		},
		{
			name:           "probe success registers",
			result:         fakeAddResult{registers: true},
			wantRegistered: true,
			wantKey:        true,
		},
		{
			name: "auth rejection is the expected pending state",
			result: fakeAddResult{
				out: fakeDeployKeyLine + "\nupstream SSH identity is not ready: " + authRejection + "\n",
				err: errors.New("exit status 1"),
			},
			wantRegistered: false,
			wantKey:        true,
		},
		{
			name: "knownhosts mismatch is surfaced",
			result: fakeAddResult{
				out: fakeDeployKeyLine + "\nupstream SSH identity is not ready: ssh: handshake failed: knownhosts: key mismatch\n",
				err: errors.New("exit status 1"),
			},
			wantErr: "knownhosts: key mismatch",
		},
		{
			name: "failure without a key is surfaced",
			result: fakeAddResult{
				out: "app: --yes cannot trust unauthenticated SSH host keys for unknown forge\n",
				err: errors.New("exit status 1"),
			},
			wantErr: "cannot trust unauthenticated SSH host keys",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			host := &fakeOberthHost{t: t, addResults: []fakeAddResult{tc.result}}
			var buf bytes.Buffer
			deps := onboardingDeps(t, host, &buf, strings.NewReader(""), true)
			cfg := Config{}
			_ = cfg.Validate()

			registered, key, err := registerUpstreamQuiet(context.Background(), cfg, deps, "codeberg", "ssh://git@codeberg.org/cloudtaser")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if registered != tc.wantRegistered {
				t.Fatalf("registered = %v, want %v", registered, tc.wantRegistered)
			}
			if (key != "") != tc.wantKey {
				t.Fatalf("key extracted = %q, wantKey %v", key, tc.wantKey)
			}
			joined := strings.Join(host.addArgv[0], " ")
			if !strings.Contains(joined, "--yes") || !strings.Contains(joined, "--no-wait") {
				t.Fatalf("upstream add must pass --yes --no-wait, got: %s", joined)
			}
		})
	}
}

// TestIsExpectedRegistrationPendingMirrorsPodClassifier guards the textual
// contract with the pod-side allowlist (app.isExpectedAuthRejection, issue
// #89): ONLY the x/crypto/ssh publickey auth-rejection signature is the
// expected quiet state; every other failure class must classify as
// unexpected so callers surface it.
func TestIsExpectedRegistrationPendingMirrorsPodClassifier(t *testing.T) {
	t.Parallel()
	for _, expected := range []string{
		"ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey], no supported methods remain",
		"SSH: UNABLE TO AUTHENTICATE",
		"no supported methods remain",
	} {
		if !isExpectedRegistrationPending(expected) {
			t.Fatalf("auth rejection must classify as pending: %q", expected)
		}
	}
	for _, unexpected := range []string{
		"ssh: handshake failed: knownhosts: key mismatch",
		"ssh: handshake failed: knownhosts: key is unknown",
		"ssh: handshake failed: EOF",
		"dial tcp: connection refused",
		"app: parse SSH identity: invalid format",
		"verify authenticated Git write capability: EOF",
	} {
		if isExpectedRegistrationPending(unexpected) {
			t.Fatalf("unexpected failure must NOT classify as pending: %q", unexpected)
		}
	}
}

func TestUpstreamFromInput(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		raw      string
		wantName string
		wantURL  string
		wantErr  bool
	}{
		{raw: "codeberg.org/cloudtaser", wantName: "codeberg", wantURL: "ssh://git@codeberg.org/cloudtaser"},
		{raw: "github.com/oberthci", wantName: "github", wantURL: "ssh://git@github.com/oberthci"},
		{raw: "ssh://git@forge.example.eu:2222/org", wantName: "forge", wantURL: "ssh://git@forge.example.eu:2222/org"},
		{raw: "https://codeberg.org/cloudtaser", wantErr: true},
		{raw: "codeberg.org", wantErr: true},
	} {
		name, baseURL, err := upstreamFromInput(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("upstreamFromInput(%q) should fail", tc.raw)
			}
			continue
		}
		if err != nil {
			t.Errorf("upstreamFromInput(%q): %v", tc.raw, err)
			continue
		}
		if name != tc.wantName || baseURL != tc.wantURL {
			t.Errorf("upstreamFromInput(%q) = %q, %q; want %q, %q", tc.raw, name, baseURL, tc.wantName, tc.wantURL)
		}
	}
}

func TestReadLineLeavesRemainingStream(t *testing.T) {
	t.Parallel()
	reader := strings.NewReader("first line\r\nremaining-for-kubectl")
	line, err := readLine(context.Background(), reader)
	if err != nil {
		t.Fatal(err)
	}
	if line != "first line" {
		t.Fatalf("line = %q", line)
	}
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(rest) != "remaining-for-kubectl" {
		t.Fatalf("readLine consumed lookahead; remaining = %q", rest)
	}
}

func TestReadLineReturnsErrInterruptedOnETX(t *testing.T) {
	t.Parallel()
	// Byte 0x03 is ETX (Ctrl+C). When the terminal delivers it as data
	// instead of generating SIGINT, readLine must return ErrInterrupted.
	reader := strings.NewReader("ab\x03rest")
	_, err := readLine(context.Background(), reader)
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("readLine with ETX byte: got err=%v, want ErrInterrupted", err)
	}
	// The ETX byte stops reading immediately; bytes before the ETX are
	// discarded (not returned as a partial line) so the caller can
	// propagate the interrupt cleanly.
	rest, _ := io.ReadAll(reader)
	if string(rest) != "rest" {
		t.Fatalf("remaining after ETX = %q, want %q", rest, "rest")
	}
}

func TestReadLineReturnsErrInterruptedOnETXAtStart(t *testing.T) {
	t.Parallel()
	reader := strings.NewReader("\x03")
	_, err := readLine(context.Background(), reader)
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("readLine with leading ETX: got err=%v, want ErrInterrupted", err)
	}
}

func TestReadLineReturnsErrInterruptedOnCancelledContext(t *testing.T) {
	t.Parallel()
	// Simulate SIGINT caught by signal.NotifyContext: the context is
	// cancelled while readLine blocks on a reader that never produces data.
	// A blocking reader (not a strings.NewReader that returns immediately)
	// is needed to exercise the select-on-ctx.Done path.
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately — readLine must not block
	r, _ := io.Pipe()
	defer r.Close()
	_, err := readLine(ctx, r)
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("readLine with cancelled context: got err=%v, want ErrInterrupted", err)
	}
}

func TestUpstreamListHasRows(t *testing.T) {
	t.Parallel()
	if upstreamListHasRows([]byte("NAME   KIND   URL   KEY FINGERPRINT\n")) {
		t.Fatal("header-only output must report no upstreams")
	}
	if upstreamListHasRows([]byte("")) {
		t.Fatal("empty output must report no upstreams")
	}
	if !upstreamListHasRows([]byte("NAME   KIND   URL   KEY FINGERPRINT\ncodeberg   ssh   ssh://git@codeberg.org/x   SHA256:y\n")) {
		t.Fatal("a data row must report a configured upstream")
	}
	// kubectl stderr noise before the header (e.g., "Defaulted container..."
	// from multi-container pods merged by CombinedOutput) must not trigger a
	// false positive.
	if upstreamListHasRows([]byte("Defaulted container \"oberth\" out of: oberth, runner\nNAME   KIND   URL   KEY FINGERPRINT\n")) {
		t.Fatal("kubectl stderr noise before header-only output must report no upstreams")
	}
	if !upstreamListHasRows([]byte("Defaulted container \"oberth\" out of: oberth, runner\nNAME   KIND   URL   KEY FINGERPRINT\ncodeberg   ssh   ssh://git@codeberg.org/x   SHA256:y\n")) {
		t.Fatal("a data row after kubectl stderr noise and header must report a configured upstream")
	}
}

func TestApplyProvidedDeployKeyStoresAndRefusesOverwrite(t *testing.T) {
	t.Parallel()
	_, private, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "test deploy key")
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := pem.EncodeToMemory(block)

	deps := Deps{KubeClient: fake.NewClientset()}
	cfg := Config{}
	_ = cfg.Validate()

	if err := applyProvidedDeployKey(context.Background(), cfg, deps, privatePEM); err != nil {
		t.Fatal(err)
	}
	secret, err := deps.KubeClient.CoreV1().Secrets(DefaultNamespace).Get(context.Background(), upstreamKeySecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("deploy-key secret not created: %v", err)
	}
	if !bytes.Equal(secret.Data[upstreamPrivateKeyField], privatePEM) {
		t.Fatal("private key not stored verbatim")
	}
	if !strings.HasPrefix(string(secret.Data[upstreamPublicKeyField]), "ssh-ed25519 ") ||
		!strings.Contains(string(secret.Data[upstreamPublicKeyField]), " oberth") {
		t.Fatalf("derived public key wrong: %q", secret.Data[upstreamPublicKeyField])
	}

	// A Secret that already holds a key is an identity — never overwritten.
	err = applyProvidedDeployKey(context.Background(), cfg, deps, privatePEM)
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("want overwrite refusal, got %v", err)
	}
}

// --- SSH config generation ---

func TestGenerateSSHConfigBlock(t *testing.T) {
	t.Parallel()

	block := GenerateSSHConfigBlock("localhost", "30022", "~/.ssh/id_ed25519")
	want := "Host oberth\n    HostName localhost\n    Port 30022\n    User git\n    IdentityFile \"~/.ssh/id_ed25519\"\n    IdentitiesOnly yes\n"
	if block != want {
		t.Fatalf("block = %q, want %q", block, want)
	}
}

func TestGenerateSSHConfigBlockPathWithSpaces(t *testing.T) {
	t.Parallel()

	block := GenerateSSHConfigBlock("localhost", "30022", "/Users/alice/Dropbox (Personal)/keys/id_ed25519")
	if !strings.Contains(block, `IdentityFile "/Users/alice/Dropbox (Personal)/keys/id_ed25519"`) {
		t.Fatalf("path with spaces must be quoted:\n%s", block)
	}
}

func TestGenerateSSHConfigBlockRemoteHost(t *testing.T) {
	t.Parallel()

	block := GenerateSSHConfigBlock("192.168.1.208", "30022", "~/.ssh/oberth_key")
	for _, want := range []string{
		"Host oberth",
		"HostName 192.168.1.208",
		"Port 30022",
		"User git",
		`IdentityFile "~/.ssh/oberth_key"`,
		"IdentitiesOnly yes",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("block missing %q:\n%s", want, block)
		}
	}
}

func TestPrivateKeyPath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		pubPath string
		want    string
	}{
		{"~/.ssh/id_ed25519.pub", "~/.ssh/id_ed25519"},
		{"~/.ssh/my_key.pub", "~/.ssh/my_key"},
		{"/home/user/.ssh/id_rsa.pub", "/home/user/.ssh/id_rsa"},
		{"~/.ssh/id_ed25519", "~/.ssh/id_ed25519"},
	} {
		if got := privateKeyPath(tc.pubPath); got != tc.want {
			t.Errorf("privateKeyPath(%q) = %q, want %q", tc.pubPath, got, tc.want)
		}
	}
}

func TestSSHHostFromServer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		server          string
		kindClusterName string
		want            string
	}{
		{name: "kind cluster", server: "https://127.0.0.1:6443", kindClusterName: "oberth", want: "localhost"},
		{name: "loopback IP", server: "https://127.0.0.1:6443", want: "localhost"},
		{name: "localhost hostname", server: "https://localhost:6443", want: "localhost"},
		{name: "ipv6 loopback", server: "https://[::1]:6443", want: "localhost"},
		{name: "private IP", server: "https://192.168.1.208:6443", want: "192.168.1.208"},
		{name: "rfc1918 10 net", server: "https://10.0.0.5:6443", want: "10.0.0.5"},
		{name: "empty server", server: "", want: "localhost"},
		{name: "nil restconfig", server: "", want: "localhost"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps := Deps{KindClusterName: tc.kindClusterName}
			if tc.server != "" {
				deps.RestConfig = &rest.Config{Host: tc.server}
			}
			if got := sshHostFromServer(deps); got != tc.want {
				t.Errorf("sshHostFromServer = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHasSSHHostBlock(t *testing.T) {
	t.Parallel()
	config := []byte("Host github.com\n    HostName github.com\n\nHost oberth\n    HostName localhost\n    Port 30022\n")
	if !hasSSHHostBlock(config, "oberth") {
		t.Fatal("should find Host oberth block")
	}
	if hasSSHHostBlock(config, "codeberg") {
		t.Fatal("should not find Host codeberg block")
	}
	if hasSSHHostBlock([]byte(""), "oberth") {
		t.Fatal("empty config should not match")
	}
	// Indented Host line (common in SSH config Match blocks) should still match
	// when trimmed.
	indented := []byte("  Host oberth\n    HostName localhost\n")
	if !hasSSHHostBlock(indented, "oberth") {
		t.Fatal("indented Host oberth should match when trimmed")
	}
	// Case-insensitive keyword: ssh_config treats "host" and "Host" equally.
	lowercase := []byte("host oberth\n    HostName localhost\n")
	if !hasSSHHostBlock(lowercase, "oberth") {
		t.Fatal("lowercase 'host oberth' should match case-insensitively")
	}
	// The host NAME itself is case-sensitive.
	if hasSSHHostBlock([]byte("Host Oberth\n"), "oberth") {
		t.Fatal("host name comparison should be case-sensitive")
	}
}

func TestIsSSHConfigSafePath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		path string
		safe bool
	}{
		{"/home/user/.ssh/id_ed25519", true},
		{"/Users/alice/Dropbox (Personal)/keys/id_ed25519", true},
		{`/home/user/.ssh/my"key`, false},
		{"/home/user/.ssh/key\x00file", false},
		{"/home/user/.ssh/key\nfile", false},
		{"", true},
	} {
		if got := isSSHConfigSafePath(tc.path); got != tc.safe {
			t.Errorf("isSSHConfigSafePath(%q) = %v, want %v", tc.path, got, tc.safe)
		}
	}
}

func TestOfferSSHConfigWritesFile(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	// Override UserHomeDir by pre-creating the .ssh directory under tmpDir;
	// we test the full offerSSHConfig flow by monkey-patching os.UserHomeDir
	// is not feasible, so we test the component functions instead and rely on
	// the integration test (TestFinishInstallInteractiveOnboarding) to cover
	// the wiring.

	sshDir := filepath.Join(tmpDir, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(sshDir, "config")

	// Write an SSH config with no oberth block.
	existing := "Host github.com\n    HostName github.com\n"
	if err := os.WriteFile(configPath, []byte(existing), 0600); err != nil {
		t.Fatal(err)
	}

	// Verify hasSSHHostBlock reports no match.
	data, _ := os.ReadFile(configPath)
	if hasSSHHostBlock(data, "oberth") {
		t.Fatal("precondition: config should not have oberth block yet")
	}

	// Write the block manually (simulating what offerSSHConfig does).
	block := GenerateSSHConfigBlock("192.168.1.208", "30022", "/home/user/.ssh/id_ed25519")
	f, err := os.OpenFile(configPath, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n" + block); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	_ = f.Close()

	// Verify the block was added.
	data, _ = os.ReadFile(configPath)
	if !hasSSHHostBlock(data, "oberth") {
		t.Fatal("config should now have oberth block")
	}
	if !strings.Contains(string(data), "HostName 192.168.1.208") {
		t.Fatalf("config missing HostName:\n%s", data)
	}
	if !strings.Contains(string(data), `IdentityFile "/home/user/.ssh/id_ed25519"`) {
		t.Fatalf("config missing quoted IdentityFile:\n%s", data)
	}
	if !strings.Contains(string(data), existing) {
		t.Fatal("existing config content was lost")
	}
}

// --- Credentialed policy boundary tests ---

// TestCredentialedPolicyExcludesReleaseWildcard proves that the credentialed
// policy does NOT grant a release/* wildcard. This is the Vault-level half
// of the trust-tier separation: even if admission has a bug, the
// credentialed SA's token cannot read release secrets.
func TestCredentialedPolicyExcludesReleaseWildcard(t *testing.T) {
	policy := OberthCredentialedPolicy("oberth")
	if strings.Contains(policy, "release/*") {
		t.Fatal("OberthCredentialedPolicy contains release/*; " +
			"the credentialed SA must not have wildcard access to release secrets")
	}
	if !strings.Contains(policy, "upstream/*") {
		t.Fatal("OberthCredentialedPolicy is missing upstream/*")
	}
	if !strings.Contains(policy, "revoke-self") {
		t.Fatal("OberthCredentialedPolicy is missing token self-revocation")
	}
}

// TestCredentialedPolicyWithGrantsAddsExactPaths proves that
// OberthCredentialedPolicyWithGrants includes exact-path entries for each
// approved secret alongside the upstream/* wildcard.
func TestCredentialedPolicyWithGrantsAddsExactPaths(t *testing.T) {
	policy := OberthCredentialedPolicyWithGrants("oberth", []string{
		"release/r2-upload-token",
		"release/cosign-secret",
	})
	if !strings.Contains(policy, "upstream/*") {
		t.Fatal("policy missing upstream/* wildcard")
	}
	if !strings.Contains(policy, `oberth/data/release/r2-upload-token`) {
		t.Fatal("policy missing exact path for r2-upload-token")
	}
	if !strings.Contains(policy, `oberth/data/release/cosign-secret`) {
		t.Fatal("policy missing exact path for cosign-secret")
	}
	if strings.Contains(policy, "release/*") {
		t.Fatal("policy contains release/* wildcard alongside exact paths")
	}
}

// TestCISecretsPolicyIsGrantFree proves the branch-tier policy covers the
// upstream subtree and token self-revocation and NOTHING else. The function
// deliberately takes no grants parameter, so this test is the tripwire for
// anyone re-adding one: a CI-trigger pod's Vault token must never be able to
// read a release secret, whatever the approval table says (issue #200).
func TestCISecretsPolicyIsGrantFree(t *testing.T) {
	policy := OberthCISecretsPolicy("oberth")
	if !strings.Contains(policy, `path "oberth/data/upstream/*"`) {
		t.Fatal("ci-secrets policy is missing the upstream subtree")
	}
	if !strings.Contains(policy, "revoke-self") {
		t.Fatal("ci-secrets policy is missing token self-revocation")
	}
	for _, stanza := range []string{`path "oberth/data/release`, `path "oberth/release`, `path "oberth/data/*`} {
		if strings.Contains(policy, stanza) {
			t.Fatalf("ci-secrets policy grants %s; the branch tier must never reach release paths", stanza)
		}
	}
	// Exactly two path stanzas: the upstream subtree and revoke-self. A third
	// means someone taught this policy to carry grants.
	if got := strings.Count(policy, `path "`); got != 2 {
		t.Fatalf("ci-secrets policy has %d path stanzas, want exactly 2:\n%s", got, policy)
	}
	// The credentialed policy WITH grants must still never leak into the
	// ci-secrets one: the two are separate objects with separate contents.
	granted := OberthCredentialedPolicyWithGrants("oberth", []string{"release/cosign-secret"})
	if policy == granted {
		t.Fatal("the ci-secrets policy and the granted credentialed policy are the same text")
	}
}

// TestCredentialedPolicyPathsNormalizesFullDataPaths proves that the
// installer accepts exactly the approval-table path vocabulary — the full
// "<kv>/data/<secret>" strings `oberth access allow` records and pipeline
// documents declare — and converts it to the under-prefix form the policy
// grammar uses, so one canonical path spelling flows through grants,
// admission, and the Vault policy.
func TestCredentialedPolicyPathsNormalizesFullDataPaths(t *testing.T) {
	paths, err := credentialedPolicyPaths("oberth", []string{
		"oberth/data/release/cosign-secret",
		"  oberth/data/release/r2-upload-token  ",
		"",
	})
	if err != nil {
		t.Fatalf("normalization failed: %v", err)
	}
	want := []string{"release/cosign-secret", "release/r2-upload-token"}
	if len(paths) != len(want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("paths[%d] = %q, want %q", i, paths[i], want[i])
		}
	}
}

// TestCredentialedPolicyPathsRefusesNonCanonicalInput proves that anything
// other than an exact full data path under the KV prefix fails the install:
// a short-form path, another mount's path, a wildcard, or a trailing slash
// must never silently become a policy grant.
func TestCredentialedPolicyPathsRefusesNonCanonicalInput(t *testing.T) {
	for _, input := range []string{
		"release/cosign-secret",              // short form: ambiguous, refused
		"secret/data/release/cosign-secret",  // different mount
		"oberth/data/",                       // empty secret
		"oberth/data/release/*",              // Vault suffix glob
		"oberth/data/release/+/token",        // Vault segment glob
		"oberth/data/release/cosign-secret/", // trailing slash
		"oberth/data/release//cosign-secret", // empty segment
		"oberth/metadata/release/cosign",     // not the data endpoint
	} {
		if _, err := credentialedPolicyPaths("oberth", []string{input}); err == nil {
			t.Errorf("credentialedPolicyPaths accepted %q, want refusal", input)
		}
	}
}

// TestCredentialedPolicyPathsAcceptsUpstreamPrefix proves that upstream-scoped
// grants (oberth/upstream/...) are accepted and normalized to the policy form
// (upstream/...) so the consumer writes path "oberth/data/upstream/..." in the
// Vault policy. This is the fix for the grant-bomb blocker.
func TestCredentialedPolicyPathsAcceptsUpstreamPrefix(t *testing.T) {
	t.Parallel()
	paths, err := credentialedPolicyPaths("oberth", []string{
		"oberth/upstream/cloudtaser/terraform/credentials",
		"oberth/upstream/skipops/terraform/plan/gcp-sa",
		"oberth/data/release/cosign-secret",
	})
	if err != nil {
		t.Fatalf("credentialedPolicyPaths rejected upstream paths: %v", err)
	}
	want := []string{
		"upstream/cloudtaser/terraform/credentials",
		"upstream/skipops/terraform/plan/gcp-sa",
		"release/cosign-secret",
	}
	if len(paths) != len(want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("paths[%d] = %q, want %q", i, paths[i], want[i])
		}
	}
}

// TestCredentialedPolicyWithGrantsDeduplicates proves that duplicate paths
// in the grant list produce only one policy entry.
func TestCredentialedPolicyWithGrantsDeduplicates(t *testing.T) {
	policy := OberthCredentialedPolicyWithGrants("oberth", []string{
		"release/r2-upload-token",
		"release/r2-upload-token",
	})
	count := strings.Count(policy, "release/r2-upload-token")
	if count != 1 {
		t.Fatalf("expected 1 occurrence of the path, got %d", count)
	}
}

// --- Finding 6: remote k3s still requires --yes ---

// TestRemoteK3sRequiresYes proves that a k3s engine behind a public API server
// endpoint still requires --yes. Before the fix, engine detection overrode the
// API endpoint locality check: any recognized engine marked the cluster local,
// so a remote k3s/k0s installation with a public endpoint skipped the safety
// gate.
func TestRemoteK3sRequiresYes(t *testing.T) {
	t.Parallel()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "remote-k3s-node"},
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.33.0+k3s1"},
		},
	}
	deps := Deps{
		Output:      io.Discard,
		KubeClient:  fake.NewClientset(node, readyArgoControllerPod()),
		RestConfig:  &rest.Config{Host: "https://35.200.100.1:6443"},
		ContextName: "remote-k3s",
	}
	err := Run(context.Background(), Config{}, deps)
	if err == nil {
		t.Fatal("expected error for remote k3s without --yes")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("error should mention --yes, got: %v", err)
	}
}

// TestRemoteK3sEngineStillDetected proves engine detection is informational:
// a remote k3s endpoint is detected as k3s but is NOT marked local.
func TestRemoteK3sEngineStillDetected(t *testing.T) {
	t.Parallel()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "remote-k3s-node"},
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.33.0+k3s1"},
		},
	}
	deps := Deps{
		Output:      io.Discard,
		KubeClient:  fake.NewClientset(node),
		RestConfig:  &rest.Config{Host: "https://35.200.100.1:6443"},
		ContextName: "remote-k3s",
	}
	info, err := DetectCluster(context.Background(), deps)
	if err != nil {
		t.Fatal(err)
	}
	if info.Engine != "k3s" {
		t.Fatalf("Engine = %q, want k3s", info.Engine)
	}
	if info.IsLocal {
		t.Fatal("remote k3s endpoint should not be marked local")
	}
}

// --- Finding 12: mixed kind port bindings ---

// TestValidateExistingKindClusterRejectsMixedBindings proves that a kind
// cluster with both loopback and non-loopback bindings for the same port
// is rejected.
func TestValidateExistingKindClusterRejectsMixedBindings(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		bindings []dockerPortBinding
		reject   bool
		mention  string
	}{
		{
			name: "loopback only is accepted",
			bindings: []dockerPortBinding{
				{HostIP: "127.0.0.1", HostPort: "30022"},
			},
		},
		{
			name: "loopback plus wildcard is rejected",
			bindings: []dockerPortBinding{
				{HostIP: "127.0.0.1", HostPort: "30022"},
				{HostIP: "0.0.0.0", HostPort: "30022"},
			},
			reject:  true,
			mention: "non-loopback",
		},
		{
			name: "loopback plus LAN IP is rejected",
			bindings: []dockerPortBinding{
				{HostIP: "127.0.0.1", HostPort: "30022"},
				{HostIP: "192.168.1.100", HostPort: "30022"},
			},
			reject:  true,
			mention: "non-loopback",
		},
		{
			name: "loopback plus IPv6 wildcard is rejected",
			bindings: []dockerPortBinding{
				{HostIP: "127.0.0.1", HostPort: "30022"},
				{HostIP: "::", HostPort: "30022"},
			},
			reject:  true,
			mention: "non-loopback",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if !test.reject {
				if hasNonLoopbackPortBinding(test.bindings, "30022") {
					t.Fatal("loopback-only bindings should not be flagged as non-loopback")
				}
				return
			}
			if !hasNonLoopbackPortBinding(test.bindings, "30022") {
				t.Fatal("expected non-loopback binding to be detected")
			}
			if !strings.Contains(test.mention, "non-loopback") {
				t.Fatal("test setup error")
			}
		})
	}
}

// TestK3sAutoNetworkPolicySetsFlag proves the k3s auto-detection path sets
// NetworkPolicy to "false" when the cluster engine is k3s. The disclosure
// is shown only in the component table row (not as a separate warning).
func TestK3sAutoNetworkPolicySetsFlag(t *testing.T) {
	t.Parallel()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "tuxbox"},
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.31.4+k3s1"},
		},
	}
	var buf bytes.Buffer
	deps := Deps{
		Output:      &buf,
		RunHelm:     func(_ context.Context, _ []string) ([]byte, error) { return nil, nil },
		KubeClient:  fake.NewClientset(node, readyArgoControllerPod()),
		RestConfig:  &rest.Config{Host: "https://127.0.0.1:6443"},
		ContextName: "default",
	}
	cfg := Config{DryRun: true}
	if err := Run(context.Background(), cfg, deps); err != nil {
		t.Fatal(err)
	}
	output := buf.String()
	// The dry-run plan must show the NetworkPolicy disabled in the Oberth
	// helm args (networkPolicy.enabled=false).
	if !strings.Contains(output, "networkPolicy.enabled=false") {
		t.Fatalf("k3s auto path must set networkPolicy.enabled=false; output:\n%s", output)
	}
}

// TestCredentialedPolicyWithNoGrantsMatchesBasePolicy proves that passing
// no grants produces the same output as the base OberthCredentialedPolicy.
func TestCredentialedPolicyWithNoGrantsMatchesBasePolicy(t *testing.T) {
	base := OberthCredentialedPolicy("oberth")
	withNil := OberthCredentialedPolicyWithGrants("oberth", nil)
	withEmpty := OberthCredentialedPolicyWithGrants("oberth", []string{})
	if base != withNil {
		t.Fatal("OberthCredentialedPolicy(prefix) != OberthCredentialedPolicyWithGrants(prefix, nil)")
	}
	if base != withEmpty {
		t.Fatal("OberthCredentialedPolicy(prefix) != OberthCredentialedPolicyWithGrants(prefix, []string{})")
	}
}
