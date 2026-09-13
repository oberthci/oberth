package argojob

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/utils/ptr"
)

const nonrootControllerFixtureDirectory = "testdata/nonroot-argo4.0.8"

// These are raw observations from the authenticated controller, not desired
// Pods assembled by this test. Upgrading Argo requires new controller proof.
func nonrootControllerFixture(t *testing.T, name string) *corev1.Pod {
	t.Helper()
	var provenance struct {
		Path, Version, Sum, GoModSum string
		Files                        map[string]string `json:"files"`
	}
	read := func(path string) []byte {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if err := json.Unmarshal(read(filepath.Join(nonrootControllerFixtureDirectory, "provenance.json")), &provenance); err != nil {
		t.Fatal(err)
	}
	module := provenance.Path + " " + provenance.Version
	if !strings.Contains(string(read("../../go.mod")), "\t"+module+"\n") {
		t.Fatal("Argo module version changed; regenerate controller fixtures with actual Pod proof")
	}
	sums := "\n" + string(read("../../go.sum"))
	for _, line := range []string{module + " " + provenance.Sum, module + "/go.mod " + provenance.GoModSum} {
		if !strings.Contains(sums, "\n"+line+"\n") {
			t.Fatal("Argo checksum changed; controller fixtures are stale")
		}
	}
	data := read(filepath.Join(nonrootControllerFixtureDirectory, name))
	digest := sha256.Sum256(data)
	if provenance.Files[name] != hex.EncodeToString(digest[:]) {
		t.Fatalf("controller fixture %s failed integrity check", name)
	}
	var pod corev1.Pod
	if err := json.Unmarshal(data, &pod); err != nil {
		t.Fatal(err)
	}
	return &pod
}

// Rehydrate the inputs owned by CURRENT Build, then apply CURRENT patch at the
// captured controller boundary. Only the documented fixed v4.0.8 mutations after
// that boundary are modeled; this is not a replacement controller implementation.
func nonrootApplyControllerFixture(t *testing.T, before *corev1.Pod, wf *wfv1.Workflow) *corev1.Pod {
	t.Helper()
	pod := before.DeepCopy()
	tmpl := &wf.Spec.Templates[0]
	main := tmpl.Container
	if tmpl.Script != nil {
		main = &tmpl.Script.Container
	}
	current := main.DeepCopy()
	current.Name = "main"
	for _, env := range pod.Spec.Containers[1].Env {
		if strings.HasPrefix(env.Name, "ARGO_") {
			current.Env = append(current.Env, env)
		}
	}
	if tmpl.Script != nil {
		current.Args = append(current.Args, "/argo/staging/script")
		current.VolumeMounts = append(current.VolumeMounts, corev1.VolumeMount{Name: "argo-staging", MountPath: "/argo/staging"})
	}
	pod.Spec.Containers[1] = *current
	pod.Spec.SecurityContext = tmpl.SecurityContext.DeepCopy()
	pod.Spec.ServiceAccountName = tmpl.ServiceAccountName
	pod.Spec.AutomountServiceAccountToken = tmpl.AutomountServiceAccountToken
	// Preserve upstream-generated executor/token volumes, replace source/scratch
	// definitions with current Build values. Any unexpected new volume is compared.
	for _, volume := range wf.Spec.Volumes {
		found := false
		for i := range pod.Spec.Volumes {
			if pod.Spec.Volumes[i].Name == volume.Name {
				pod.Spec.Volumes[i] = volume
				found = true
			}
		}
		if !found && volume.Name == nonrootTmpVolume {
			pod.Spec.Volumes = append(pod.Spec.Volumes, volume)
		}
	}
	wait := &pod.Spec.Containers[0]
	var mirrors []corev1.VolumeMount
	for _, mount := range wait.VolumeMounts {
		if !strings.HasPrefix(mount.MountPath, "/mainctrfs/") {
			mirrors = append(mirrors, mount)
		}
	}
	for _, mount := range current.VolumeMounts {
		mount.MountPath = "/mainctrfs" + mount.MountPath
		mount.ReadOnly = false // Actual addOutputArtifactsVolumes behavior before patch.
		mirrors = append(mirrors, mount)
	}
	wait.VolumeMounts = mirrors
	templateCopy := tmpl.DeepCopy()
	templateCopy.Inputs = wfv1.Inputs{Artifacts: templateCopy.Inputs.Artifacts}
	encoded, err := json.Marshal(templateCopy)
	if err != nil {
		t.Fatal(err)
	}
	for i := range pod.Spec.InitContainers[0].Env {
		if pod.Spec.InitContainers[0].Env[i].Name == "ARGO_TEMPLATE" {
			pod.Spec.InitContainers[0].Env[i].Value = string(encoded)
		}
	}
	spec, err := json.Marshal(pod.Spec)
	if err != nil {
		t.Fatal(err)
	}
	patched, err := strategicpatch.StrategicMergePatch(spec, []byte(tmpl.PodSpecPatch), corev1.PodSpec{})
	if err != nil {
		t.Fatal(err)
	}
	var patchedSpec corev1.PodSpec
	if err := json.Unmarshal(patched, &patchedSpec); err != nil {
		t.Fatal(err)
	}
	pod.Spec = patchedSpec
	// workflowpod.go:417–456, after processPodSpecPatch. Artifacts, custom
	// termination grace, dynamic commands and offload are outside this fixture.
	if pod.Spec.TerminationGracePeriodSeconds != nil {
		t.Fatal("fixture needs new controller proof for termination-grace mutation")
	}
	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		switch container.Name {
		case "main":
			if len(container.Command) == 0 {
				t.Fatal("fixture cannot model image entrypoint lookup")
			}
			container.Command = append([]string{"/var/run/argo/argoexec", "emissary", "--loglevel", "info", "--log-format", "text", "--gloglevel", "0", "--"}, container.Command...)
		case "wait":
			container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "tmp-dir-argo", MountPath: "/tmp", SubPath: "0"})
		default:
			t.Fatal("fixture cannot model extra container")
		}
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "var-run-argo", MountPath: "/var/run/argo"})
		args, marshalErr := json.Marshal(container.Args)
		if marshalErr != nil || len(args) > 131072 {
			t.Fatal("args would enter unmodeled Argo offload path")
		}
	}
	for _, container := range pod.Spec.InitContainers {
		for _, env := range container.Env {
			if env.Name == "ARGO_TEMPLATE" && len(env.Value) > 131072 {
				t.Fatal("template would enter Argo offload path and add a ConfigMap mount")
			}
		}
	}
	return pod
}

func TestNonrootCurrentPatchMatchesObservedControllerPods(t *testing.T) {
	for _, shape := range []string{"container", "script"} {
		t.Run(shape, func(t *testing.T) {
			before := nonrootControllerFixture(t, shape+"-before-patch.json")
			observed := nonrootControllerFixture(t, shape+"-submitted.json")
			wf := nonrootBuiltFixture(t, shape == "script")
			current := nonrootApplyControllerFixture(t, before, wf)
			got, err := json.MarshalIndent(current, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			want, err := json.MarshalIndent(observed, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				path := filepath.Join(t.TempDir(), "current-pod.json")
				if err := os.WriteFile(path, got, 0o600); err != nil {
					t.Fatal(err)
				}
				gotLines, wantLines := strings.Split(string(got), "\n"), strings.Split(string(want), "\n")
				for i := 0; i < len(gotLines) && i < len(wantLines); i++ {
					if gotLines[i] != wantLines[i] {
						t.Fatalf("current %s Pod differs at line %d: got %s; want %s (%s)", shape, i+1, gotLines[i], wantLines[i], path)
					}
				}
				t.Fatalf("current %s Pod has different size: %s", shape, path)
			}
			// Missing explicit clearing must expose hostile controller defaults;
			// the frozen before-Pod actually contains these defaults.
			wf.Spec.Templates[0].PodSpecPatch = strings.ReplaceAll(wf.Spec.Templates[0].PodSpecPatch, `"privileged":null,`, "")
			unsafe := nonrootApplyControllerFixture(t, before, wf)
			if reflect.DeepEqual(unsafe.Spec, observed.Spec) || unsafe.Spec.Containers[0].SecurityContext.Privileged == nil || !*unsafe.Spec.Containers[0].SecurityContext.Privileged {
				t.Fatal("negative control did not expose inherited privileged executor context")
			}
		})
	}
}

func TestNonrootExecutorContextWorksWhenOriginallyAbsent(t *testing.T) {
	for _, shape := range []string{"container", "script"} {
		before := nonrootControllerFixture(t, shape+"-before-patch.json")
		before.Spec.InitContainers[0].SecurityContext = nil
		before.Spec.Containers[0].SecurityContext = nil
		pod := nonrootApplyControllerFixture(t, before, nonrootBuiltFixture(t, shape == "script"))
		for _, c := range []corev1.Container{pod.Spec.InitContainers[1], pod.Spec.Containers[0]} {
			if !reflect.DeepEqual(c.SecurityContext, nonrootContainerSecurity(nonrootUID)) {
				t.Fatalf("absent %s context was not fully installed", c.Name)
			}
		}
		// Also apply the server patch to a genuinely absent Pod context. The
		// normal controller gets the current template context, but this control
		// prevents the same absent-map bug in the Pod-level replacement.
		before.Spec.SecurityContext = nil
		encoded, err := json.Marshal(before.Spec)
		if err != nil {
			t.Fatal(err)
		}
		wf := nonrootBuiltFixture(t, shape == "script")
		merged, err := strategicpatch.StrategicMergePatch(encoded, []byte(wf.Spec.Templates[0].PodSpecPatch), corev1.PodSpec{})
		if err != nil {
			t.Fatal(err)
		}
		var actual corev1.PodSpec
		if err := json.Unmarshal(merged, &actual); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(actual.SecurityContext, wf.Spec.Templates[0].SecurityContext) {
			t.Fatal("absent Pod context was not fully installed")
		}
	}
}

func TestNonrootContextReplacementCoversEverySecurityField(t *testing.T) {
	wf := nonrootBuiltFixture(t, false)
	var patch map[string]any
	if err := json.Unmarshal([]byte(wf.Spec.Templates[0].PodSpecPatch), &patch); err != nil {
		t.Fatal(err)
	}
	security := patch["containers"].([]any)[0].(map[string]any)["securityContext"].(map[string]any)
	for _, check := range []struct {
		value  any
		fields map[string]any
	}{
		{corev1.PodSecurityContext{}, patch["securityContext"].(map[string]any)},
		{corev1.SecurityContext{}, security},
		{corev1.Capabilities{}, security["capabilities"].(map[string]any)},
		{corev1.SeccompProfile{}, security["seccompProfile"].(map[string]any)},
	} {
		typ := reflect.TypeOf(check.value)
		for i := 0; i < typ.NumField(); i++ {
			name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
			if _, ok := check.fields[name]; !ok {
				t.Fatalf("new %s.%s field needs explicit nonroot replacement semantics", typ.Name(), name)
			}
		}
	}
}

func TestNonrootRecoveryAcceptsActualControllerPersistedSpec(t *testing.T) {
	for _, shape := range []string{"container", "script"} {
		// Authentic module, checksum and fixture-integrity guards apply here too.
		_ = nonrootControllerFixture(t, shape+"-before-patch.json")
		file := shape + "-persisted-workflow.json"
		data, err := os.ReadFile(filepath.Join(nonrootControllerFixtureDirectory, file))
		if err != nil {
			t.Fatal(err)
		}
		var provenance struct {
			Files map[string]string `json:"files"`
		}
		encoded, err := os.ReadFile(filepath.Join(nonrootControllerFixtureDirectory, "provenance.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &provenance); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(data)
		if provenance.Files[file] != hex.EncodeToString(digest[:]) {
			t.Fatal("persisted Workflow fixture integrity mismatch")
		}
		var existing wfv1.Workflow
		if err := json.Unmarshal(data, &existing); err != nil {
			t.Fatal(err)
		}
		if existing.Status.Phase != wfv1.WorkflowRunning || len(existing.Status.Nodes) == 0 {
			t.Fatal("fixture did not undergo actual supported reconciliation")
		}
		intended := nonrootBuiltFixture(t, shape == "script")
		if err := sameSubmission(&existing, intended); err != nil {
			t.Fatal(err)
		}
		// Recovery's exact spec check must still notice policy drift after the
		// controller's legitimate status and metadata mutations.
		existing.Spec.Templates[0].SecurityContext.RunAsUser = ptr.To(int64(0))
		if err := sameSubmission(&existing, intended); err == nil {
			t.Fatal("persisted controller metadata hid root policy drift")
		}
	}
}
