package argojob

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

const publicArtifactsSecretPath = "oberth/data/release/cloudflare-oberth-workers-token"

func builtOberthRelease(t *testing.T) *wfv1.Workflow {
	t.Helper()
	config := oberthConfig()
	config.ReleaseCacheRoot = "/var/cache/oberth/release"
	workflow, err := Build(config, Request{
		RunID: "run-public", Name: "oberth-oberth-run-public-aabbccddeeff",
		Repo: "oberth", UpstreamOrg: "acme", UpstreamName: "github", Ref: "v1.2.3", SHA: testSHA,
		Trigger: periapsis.TriggerRelease, Source: loadPipeline(t, "release.yaml"),
		ApprovedSecrets: oberthReleaseApprovedSecrets(),
		SourceVolume: SourceVolume{
			ClaimName: "oberth-oberth-run-public-aabbccddeeff-src", SubPath: "src",
			ArtifactsSubPath: "artifacts", VaultCASubPath: "vault-ca", BinarySubPath: "bin",
		},
	})
	if err != nil {
		t.Fatalf("admit release.yaml: %v", err)
	}
	return workflow
}

func releaseTemplate(t *testing.T, workflow *wfv1.Workflow, name string) *wfv1.Template {
	t.Helper()
	for index := range workflow.Spec.Templates {
		if workflow.Spec.Templates[index].Name == name {
			return &workflow.Spec.Templates[index]
		}
	}
	t.Fatalf("release.yaml has no template %q", name)
	return nil
}

func mountNames(container *corev1.Container) map[string]corev1.VolumeMount {
	mounts := map[string]corev1.VolumeMount{}
	for _, mount := range container.VolumeMounts {
		mounts[mount.Name] = mount
	}
	return mounts
}

func TestOberthPublicArtifactsLeafIsolation(t *testing.T) {
	workflow := builtOberthRelease(t)

	declared, err := argoworkflow.DeclaredSecretPaths(workflow)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(strings.Join(declared, ","), publicArtifactsSecretPath); count != 1 {
		t.Fatalf("release.yaml declares %s %d times, want once", publicArtifactsSecretPath, count)
	}

	forbiddenClaims := []string{"bootstrap-tools", "bootstrap-test-tools", "bootstrap-output", "bootstrap-runtime-receipts"}
	credentialMounts := []string{ReleaseTokenVolumeName, SecretsVolumeName}
	sharedMounts := []string{CacheVolumeName, ArtifactsVolumeName}

	publisher := releaseTemplate(t, workflow, "release-public-artifacts")
	if !templateUsesOberthSecretstore(publisher) {
		t.Fatal("release-public-artifacts is not the secretstore-wrapped credentialed leaf")
	}
	if paths := extractExecPaths(publisher); !reflect.DeepEqual(paths, []string{publicArtifactsSecretPath}) {
		t.Fatalf("release-public-artifacts fetches %v, want exactly [%s]", paths, publicArtifactsSecretPath)
	}
	for _, path := range extractExecPaths(publisher) {
		if strings.Contains(path, "/gar-") || strings.HasSuffix(path, "/cosign-secret") {
			t.Fatalf("release-public-artifacts fetches a publisher credential: %s", path)
		}
	}
	if args := strings.Join(publisher.Container.Args, "\n"); !strings.Contains(args, "./.oberth/release.sh\ncloudflare-phase\n{{inputs.parameters.phase}}") {
		t.Fatalf("release-public-artifacts does not run the reviewed fixed Cloudflare phase action:\n%s", args)
	}
	main := mountNames(publisher.Container)
	for _, name := range append(append([]string{"bootstrap-test-tools"}, sharedMounts...), "bootstrap-node-inputs") {
		if _, found := main[name]; found {
			t.Errorf("release-public-artifacts main container mounts %s", name)
		}
	}
	for name, path := range map[string]string{
		"bootstrap-tools":            "/tmp/oberth-tools",
		"bootstrap-output":           "/tmp/oberth-release-inputs",
		"bootstrap-runtime-receipts": "/tmp/oberth-runtime-receipts",
	} {
		if mount, found := main[name]; !found || !mount.ReadOnly || mount.MountPath != path {
			t.Errorf("release-public-artifacts %s must be read-only at %s, got %+v", name, path, mount)
		}
	}
	for _, name := range credentialMounts {
		if _, found := main[name]; !found {
			t.Errorf("release-public-artifacts main container lacks server credential mount %s", name)
		}
	}
	for _, init := range publisher.InitContainers {
		if init.Name != "verify-publisher-tools" {
			t.Fatalf("unexpected artifact publisher init: %s", init.Name)
		}
		for _, mount := range init.VolumeMounts {
			for _, forbidden := range append(append(append([]string{}, forbiddenClaims[1:]...), sharedMounts...), credentialMounts...) {
				if mount.Name == forbidden {
					t.Errorf("tokenless verifier mounts %s", forbidden)
				}
			}
		}
	}
	if len(publisher.InitContainers) != 1 {
		t.Fatal("artifact publisher must verify tools before fetching credentials")
	}
}

// TestOberthReleaseNodePinsMatchTheFetch keeps the literal fetch URL, the
// tarball pin path and the extracted-binary pin in one reviewed agreement.
func TestOberthReleaseNodePinsMatchTheFetch(t *testing.T) {
	workflow, err := argoworkflow.Decode(loadPipeline(t, "release.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	readPin := func(name string) string {
		body, err := os.ReadFile(filepath.Join(repositoryRoot(t), ".oberth", "pins", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	url := strings.TrimSuffix(readPin("node.url"), "\n")
	if !regexp.MustCompile(`^https://nodejs\.org/dist/v[0-9]+\.[0-9]+\.[0-9]+/node-v[0-9]+\.[0-9]+\.[0-9]+-linux-x64\.tar\.gz$`).MatchString(url) {
		t.Fatalf("node.url pin %q is not an official nodejs.org linux-x64 tarball", url)
	}
	tarball := regexp.MustCompile(`^([0-9a-f]{64})  (/tmp/oberth-node-inputs/node\.tar\.gz)\n$`).FindStringSubmatch(readPin("node.sha256"))
	binary := regexp.MustCompile(`^[0-9a-f]{64}  /tmp/oberth-test-node/node/bin/node\n$`).MatchString(readPin("node-bin.sha256"))
	if tarball == nil || !binary {
		t.Fatal("node.sha256 / node-bin.sha256 must each pin exactly one reviewed production path")
	}
	fetch := releaseTemplate(t, workflow, "fetch-node").Container.Args
	if fetch[len(fetch)-1] != url || fetch[len(fetch)-2] != tarball[2] || fetch[len(fetch)-3] != "--output" {
		t.Fatalf("fetch-node must download node.url to the pinned path; args end %v", fetch[len(fetch)-3:])
	}
	for _, flag := range []string{"=https", "--tlsv1.2", "--fail"} {
		if !strings.Contains(strings.Join(fetch, " "), flag) {
			t.Errorf("fetch-node lacks %s", flag)
		}
	}
	verify := releaseTemplate(t, workflow, "verify-node").Container.Args
	if !reflect.DeepEqual(verify[len(verify)-3:], []string{"/usr/bin/sha256sum", "-c", ".oberth/pins/node.sha256"}) {
		t.Fatalf("verify-node must check the committed pin, args end %v", verify[len(verify)-3:])
	}
	// The staging steps run before the test tools are copied and before any
	// credentialed leaf, inside the fail-fast release-setup sequence.
	var setup []string
	for _, group := range releaseTemplate(t, workflow, "release-setup").Steps {
		for _, step := range group.Steps {
			setup = append(setup, step.Name)
		}
	}
	joined := strings.Join(setup, ",")
	if !strings.Contains(joined, "validate-tag,fetch-apko,verify-apko,install-apko,fetch-node,verify-node,copy-test-tools") {
		t.Fatalf("release-setup order = %s", joined)
	}
}
