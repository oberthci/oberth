package argojob

import (
	"os"
	"os/exec"
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

const websiteSecretPath = "oberth/data/release/cloudflare-oberth-workers-token"

// websiteTokenlessTemplates are the credential-free #649 templates and the one
// claim mount each may hold ("" = none; "ro" = read-only).
var websiteTokenlessTemplates = map[string]string{
	"fetch-node":               "rw",
	"verify-node":              "ro",
	"stage-website-packages":   "rw",
	"release-website-readback": "",
}

func builtOberthRelease(t *testing.T) *wfv1.Workflow {
	t.Helper()
	config := oberthConfig()
	config.ReleaseCacheRoot = "/var/cache/oberth/release"
	workflow, err := Build(config, Request{
		RunID: "run-website", Name: "oberth-oberth-run-website-aabbccddeeff",
		Repo: "oberth", UpstreamOrg: "skipops", Ref: "v1.2.3", SHA: testSHA,
		Trigger: periapsis.TriggerRelease, Source: loadPipeline(t, "release.yaml"),
		ApprovedSecrets: oberthReleaseApprovedSecrets(),
		SourceVolume: SourceVolume{
			ClaimName: "oberth-oberth-run-website-aabbccddeeff-src", SubPath: "src",
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

// TestOberthReleaseWebsiteLeafIsolation pins the #649 publication leaf through
// the real admission chain: exactly one new secret path, fetched only by the
// release-website main container, never a GAR key or the cosign secret; no
// signed publisher inputs and runtime receipts are read-only; no shared caches
// or artifacts are mounted; every other website template is tokenless.
func TestOberthReleaseWebsiteLeafIsolation(t *testing.T) {
	workflow := builtOberthRelease(t)

	declared, err := argoworkflow.DeclaredSecretPaths(workflow)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(strings.Join(declared, ","), websiteSecretPath); count != 1 {
		t.Fatalf("release.yaml declares %s %d times, want once", websiteSecretPath, count)
	}

	forbiddenClaims := []string{"bootstrap-tools", "bootstrap-test-tools", "bootstrap-output", "bootstrap-runtime-receipts"}
	credentialMounts := []string{ReleaseTokenVolumeName, SecretsVolumeName}
	sharedMounts := []string{CacheVolumeName, ArtifactsVolumeName}

	website := releaseTemplate(t, workflow, "release-website")
	if !templateUsesOberthSecretstore(website) {
		t.Fatal("release-website is not the secretstore-wrapped credentialed leaf")
	}
	if paths := extractExecPaths(website); !reflect.DeepEqual(paths, []string{websiteSecretPath}) {
		t.Fatalf("release-website fetches %v, want exactly [%s]", paths, websiteSecretPath)
	}
	for _, path := range extractExecPaths(website) {
		if strings.Contains(path, "/gar-") || strings.HasSuffix(path, "/cosign-secret") {
			t.Fatalf("release-website fetches a publisher credential: %s", path)
		}
	}
	if args := strings.Join(website.Container.Args, "\n"); !strings.Contains(args, "./.oberth/release.sh\ncloudflare-phase\n{{inputs.parameters.phase}}") {
		t.Fatalf("release-website does not run the reviewed fixed Cloudflare phase action:\n%s", args)
	}
	main := mountNames(website.Container)
	for _, name := range append(append([]string{"bootstrap-test-tools"}, sharedMounts...), "bootstrap-website-inputs") {
		if _, found := main[name]; found {
			t.Errorf("release-website main container mounts %s", name)
		}
	}
	for name, path := range map[string]string{
		"bootstrap-tools":            "/tmp/oberth-tools",
		"bootstrap-output":           "/tmp/oberth-release-inputs",
		"bootstrap-runtime-receipts": "/tmp/oberth-runtime-receipts",
	} {
		if mount, found := main[name]; !found || !mount.ReadOnly || mount.MountPath != path {
			t.Errorf("release-website %s must be read-only at %s, got %+v", name, path, mount)
		}
	}
	for _, name := range credentialMounts {
		if _, found := main[name]; !found {
			t.Errorf("release-website main container lacks server credential mount %s", name)
		}
	}
	var initNames []string
	for index := range website.InitContainers {
		container := &website.InitContainers[index].Container
		initNames = append(initNames, container.Name)
		mounts := mountNames(container)
		for _, name := range append(append(append([]string{}, forbiddenClaims...), sharedMounts...), credentialMounts...) {
			if mount, found := mounts[name]; found {
				if container.Name == "verify-publisher-tools" && name == "bootstrap-tools" && mount.ReadOnly && mount.MountPath == "/tmp/oberth-tools" {
					continue
				}
				t.Errorf("init container %s mounts %s", container.Name, name)
			}
		}
		inputs, hasInputs := mounts["bootstrap-website-inputs"]
		switch container.Name {
		case "verify-website-inputs":
			if !hasInputs || !inputs.ReadOnly || inputs.MountPath != "/tmp/oberth-website-inputs" {
				t.Errorf("verify-website-inputs must read the staged inputs read-only, got %+v", inputs)
			}
			if !strings.Contains(strings.Join(container.Args, " "), "/usr/bin/python3 -I -S -B /work/src/.oberth/verify-website-inputs.py") {
				t.Errorf("verify-website-inputs does not run the committed verifier: %v", container.Args)
			}
		case "install-website-tools":
			if hasInputs {
				t.Error("install-website-tools must use only the verifier's Pod-private copies")
			}
			if !strings.Contains(strings.Join(container.Args, "\n"), "./.oberth/release.sh\ninstall-website-tools") {
				t.Errorf("install-website-tools does not run the reviewed action: %v", container.Args)
			}
		}
	}
	if !reflect.DeepEqual(initNames, []string{"verify-publisher-tools", "verify-website-inputs", "install-website-tools"}) {
		t.Fatalf("release-website init containers = %v", initNames)
	}

	for name, claim := range websiteTokenlessTemplates {
		template := releaseTemplate(t, workflow, name)
		if !tokenDisabled(template) || templateUsesCredentialChain(template) {
			t.Errorf("%s must be a tokenless, credential-free template", name)
		}
		if workspaceMountMode(template) != "none" || !workspaceEnvDisabled(template) {
			t.Errorf("%s must refuse shared workspace mounts and environment", name)
		}
		mounts := mountNames(template.Container)
		for _, forbidden := range append(append(append([]string{}, forbiddenClaims...), sharedMounts...), credentialMounts...) {
			if _, found := mounts[forbidden]; found {
				t.Errorf("%s mounts %s", name, forbidden)
			}
		}
		for _, mount := range template.Container.VolumeMounts {
			if mount.MountPath == OberthBinMountPath || mount.MountPath == VaultCAMountPath {
				t.Errorf("%s receives server credential plumbing at %s", name, mount.MountPath)
			}
		}
		for _, variable := range template.Container.Env {
			if strings.HasPrefix(variable.Name, "VAULT_") || variable.Name == "OBERTH_VAULT_ROLE" || variable.Name == "OBERTH_ARTIFACTS" {
				t.Errorf("%s receives %s", name, variable.Name)
			}
		}
		inputs, hasInputs := mounts["bootstrap-website-inputs"]
		switch claim {
		case "":
			if hasInputs {
				t.Errorf("%s must not mount the website inputs claim", name)
			}
		case "ro", "rw":
			if !hasInputs || inputs.ReadOnly != (claim == "ro") || inputs.MountPath != "/tmp/oberth-website-inputs" {
				t.Errorf("%s website inputs mount = %+v, want %s at /tmp/oberth-website-inputs", name, inputs, claim)
			}
		}
	}

	// No other Pod may write or read the staged website inputs.
	for index := range workflow.Spec.Templates {
		template := &workflow.Spec.Templates[index]
		if _, website := websiteTokenlessTemplates[template.Name]; website || template.Name == "release-website" {
			continue
		}
		templateContainers(template, func(container *corev1.Container) {
			if _, found := mountNames(container)["bootstrap-website-inputs"]; found {
				t.Errorf("%s/%s mounts the website inputs claim", template.Name, container.Name)
			}
		})
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
	tarball := regexp.MustCompile(`^([0-9a-f]{64})  (/tmp/oberth-website-inputs/node\.tar\.gz)\n$`).FindStringSubmatch(readPin("node.sha256"))
	binary := regexp.MustCompile(`^[0-9a-f]{64}  /tmp/oberth-website/node/bin/node\n$`).MatchString(readPin("node-bin.sha256"))
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
	if !strings.Contains(joined, "validate-tag,fetch-node,verify-node,stage-website-packages,copy-test-tools") {
		t.Fatalf("release-setup order = %s", joined)
	}
}

// TestWebsitePublisherInputsAndReadbackControls runs the adversarial Python
// suite for the website source/input verifier and the public readback.
func TestWebsitePublisherInputsAndReadbackControls(t *testing.T) {
	command := exec.CommandContext(t.Context(), "/usr/bin/python3", "-I", "-S", "-B", "../../hack/test-website-release.py")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("website verifier/readback controls: %v\n%s", err, output)
	}
}
