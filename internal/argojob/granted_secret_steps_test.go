package argojob

import (
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func storeMount(m corev1.VolumeMount) bool {
	return m.Name == ReleaseTokenVolumeName || m.Name == SecretsVolumeName ||
		strings.Contains(m.MountPath, VaultCAMountPath) || strings.Contains(m.MountPath, OberthBinMountPath)
}

func assertNoStoreSettings(t *testing.T, c corev1.Container) {
	t.Helper()
	for _, e := range c.Env {
		if strings.HasPrefix(e.Name, "VAULT_") || e.Name == "OBERTH_VAULT_ROLE" {
			t.Errorf("%s received %s", c.Name, e.Name)
		}
	}
	for _, m := range c.VolumeMounts {
		if storeMount(m) {
			t.Errorf("%s received store mount %+v", c.Name, m)
		}
	}
}

func TestGrantedSecretStepContainerBoundary(t *testing.T) {
	for _, script := range []bool{false, true} {
		t.Run(map[bool]string{false: "container", true: "script"}[script], func(t *testing.T) {
			r := isolationRequest(t, func(w *wfv1.Workflow) {
				leaf := &w.Spec.Templates[0]
				leaf.Container.Command = []string{OberthBinPath, "secretstore", "exec"}
				leaf.Container.Args = []string{"--path=oberth/data/release/r2-upload-token", "--", "/bin/true"}
				helper := wfv1.UserContainer{Container: corev1.Container{Name: "steal", Image: leaf.Container.Image, Command: leaf.Container.Command, Args: leaf.Container.Args}, MirrorVolumeMounts: ptr.To(true)}
				leaf.Sidecars = []wfv1.UserContainer{helper}
				helper.Name = "prepare"
				leaf.InitContainers = []wfv1.UserContainer{helper}
				if script {
					leaf.Script = &wfv1.ScriptTemplate{Container: *leaf.Container, Source: "true"}
					leaf.Container = nil
				}
				w.Spec.Templates = append(w.Spec.Templates, wfv1.Template{Name: "ungranted", Container: &corev1.Container{Image: templateMainContainer(leaf).Image, Command: []string{"/bin/true"}}})
			})
			r.StepGrants = map[string]map[string]bool{"oberth/data/release/r2-upload-token": {"main": true}}
			config := testConfig()
			config.PerStepIdentities = map[string]PerRepoIdentityConfig{canonicalRepoKey(r.UpstreamName, r.UpstreamOrg, r.Repo) + "/.oberth/release.yaml/main": {ServiceAccountName: "step-main"}}
			wf, err := Build(config, r)
			if err != nil {
				t.Fatal(err)
			}
			leaf := &wf.Spec.Templates[0]
			main := templateMainContainer(leaf)
			mounts := map[string]bool{}
			for _, m := range main.VolumeMounts {
				mounts[m.MountPath] = true
			}
			for _, want := range []string{ReleaseTokenMountPath, SecretsMountPath, VaultCAMountPath, OberthBinMountPath} {
				if !mounts[want] {
					t.Errorf("granted main lost %s", want)
				}
			}
			assertStoreAddress(t, *main, testVaultAddress)
			env := map[string]string{}
			for _, e := range main.Env {
				env[e.Name] = e.Value

			}
			if env["VAULT_ADDR"] != config.VaultAddress || env["VAULT_CACERT"] != VaultCACertPath || env["OBERTH_VAULT_ROLE"] != "step-main" {
				t.Fatalf("granted settings: %v", env)
			}
			for _, c := range []corev1.Container{leaf.Sidecars[0].Container, leaf.InitContainers[0].Container, *wf.Spec.Templates[1].Container} {
				assertNoStoreSettings(t, c)
			}
			before := credentialWaitControllerShape(*main)
			before.InitContainers = append(before.InitContainers, leaf.InitContainers[0].Container)
			metadata, err := json.Marshal(leaf)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(metadata), testVaultAddress) {
				t.Fatal("public address unexpectedly hidden from Argo template metadata")
			}
			for i := range before.InitContainers {
				before.InitContainers[i].Env = append(before.InitContainers[i].Env, corev1.EnvVar{Name: "ARGO_TEMPLATE", Value: string(metadata)})
			}
			after := applyCredentialWaitPatch(t, before, leaf.PodSpecPatch)
			assertNativeWaitCredentialMirrors(t, before.Containers[0], after.Containers[0])
			for _, c := range after.InitContainers {
				assertNoStoreSettings(t, c)
			}
			r.StepGrants = nil
			if _, err := Build(config, r); err == nil {
				t.Fatal("missing grants admitted")
			}
		})
	}
}

func TestGrantedInlineStepCannotBorrowNamedIdentity(t *testing.T) {
	r := isolationRequest(t, func(w *wfv1.Workflow) {
		inline := w.Spec.Templates[0].DeepCopy()
		inline.Name = "publish"
		inline.Container.Command = []string{OberthBinPath, "secretstore", "exec"}
		inline.Container.Args = []string{"--path=oberth/data/release/r2-upload-token", "--", "/bin/true"}
		w.Spec.Templates[0].Container = nil
		w.Spec.Templates[0].Steps = []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "inline", Inline: inline}}}}
	})
	r.StepGrants = map[string]map[string]bool{"oberth/data/release/r2-upload-token": {"*": true, "publish": true}}
	config := testConfig()
	config.PerStepIdentities["oberth/.oberth/release.yaml/publish"] = PerRepoIdentityConfig{ServiceAccountName: "named-publish"}
	wf, err := Build(config, r)
	if err != nil {
		t.Fatal(err)
	}
	inline := wf.Spec.Templates[0].Steps[0].Steps[0].Inline
	if inline.ServiceAccountName != testCredentialedAcct {
		t.Fatalf("inline step borrowed named authority: %s", inline.ServiceAccountName)
	}
}

func TestGrantedStepInheritsTokenOptOutBeforeDelivery(t *testing.T) {
	for _, level := range []string{"workflow", "defaults", "template", "inline-parent"} {
		t.Run(level, func(t *testing.T) {
			r := isolationRequest(t, func(w *wfv1.Workflow) {
				leaf := &w.Spec.Templates[0]
				leaf.Container.Command = []string{OberthBinPath, "secretstore", "exec"}
				leaf.Container.Args = []string{"--path=oberth/data/release/r2-upload-token", "--", "/bin/true"}
				switch level {
				case "workflow":
					w.Spec.AutomountServiceAccountToken = ptr.To(false)
				case "defaults":
					w.Spec.TemplateDefaults = &wfv1.Template{AutomountServiceAccountToken: ptr.To(false)}
				case "template":
					leaf.AutomountServiceAccountToken = ptr.To(false)
				case "inline-parent":
					inline := leaf.DeepCopy()
					leaf.Container = nil
					leaf.AutomountServiceAccountToken = ptr.To(false)
					leaf.Steps = []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "inline", Inline: inline}}}}
				}
			})
			wf, err := argoworkflow.Decode(r.Source)
			if err != nil {
				t.Fatal(err)
			}
			paths, err := argoworkflow.DeclaredSecretPaths(wf)
			if err != nil {
				t.Fatal(err)
			}
			steps, err := GrantedSecretSteps(wf, paths, r.StepGrants)
			if err != nil || len(steps) != 0 || SecretStoreRequested(wf) || requestNeedsSecretStore(r) {
				t.Fatal("disabled consumer selected for delivery", steps, err)
			}
			// The existing admission policy refuses a chain that cannot authenticate.
			if built, err := Build(testConfig(), r); built != nil || err == nil || !strings.Contains(err.Error(), "disables its service-account token") {
				t.Fatalf("disabled chain reached delivery: %v, %v", built, err)
			}
		})
	}
}

func TestGrantedSecretStepFetchesFromFakeStore(t *testing.T) {
	var logins, reads, revokes atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/auth/kubernetes/login", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["role"] != testVaultCredentialedRole || body["jwt"] != "granted-step-jwt" {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		logins.Add(1)
		_, _ = w.Write([]byte(`{"auth":{"client_token":"fake-login-token","lease_duration":60}}`))
	})
	mux.HandleFunc("GET /v1/oberth/data/release/r2-upload-token", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "fake-login-token" {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		reads.Add(1)
		_, _ = w.Write([]byte(`{"data":{"data":{"value":"fetched-by-granted-step"},"metadata":{"version":1}}}`))
	})
	mux.HandleFunc("PUT /v1/auth/token/revoke-self", func(w http.ResponseWriter, r *http.Request) { revokes.Add(1); w.WriteHeader(http.StatusNoContent) })
	server := httptest.NewTLSServer(mux)
	defer server.Close()
	r := isolationRequest(t, func(w *wfv1.Workflow) {
		w.Spec.Templates[0].Container.Command = []string{OberthBinPath, "secretstore", "exec"}
		w.Spec.Templates[0].Container.Args = []string{"--path=oberth/data/release/r2-upload-token", "--", "/bin/true"}
	})
	config := testConfig()
	config.VaultAddress = server.URL
	config.VaultCACertPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	wf, err := Build(config, r)
	if err != nil {
		t.Fatal(err)
	}
	main := wf.Spec.Templates[0].Container
	env := map[string]string{}
	for _, e := range main.Env {
		env[e.Name] = e.Value

	}
	// Resolve the projected token and seeded CA subpaths to local fake mounts.
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("granted-step-jwt"), 0o600); err != nil {
		t.Fatal(err)
	}
	var ca []byte
	for _, m := range main.VolumeMounts {
		if m.MountPath == VaultCAMountPath && m.SubPath == r.SourceVolume.VaultCASubPath {
			ca = []byte(config.VaultCACertPEM)
		}
	}
	if len(ca) == 0 || env["VAULT_CACERT"] != VaultCACertPath {
		t.Fatal("CA delivery absent")
	}
	client, err := secretstore.New(secretstore.Config{Address: env["VAULT_ADDR"], Role: env["OBERTH_VAULT_ROLE"], CACertPEM: ca, ServiceAccountTokenPath: tokenPath})
	if err != nil {
		t.Fatal(err)
	}
	values, err := client.FetchKV(t.Context(), []string{"oberth/data/release/r2-upload-token"})
	if err != nil {
		t.Fatal(err)
	}
	if string(values["oberth/data/release/r2-upload-token"]["value"]) != "fetched-by-granted-step" || logins.Load() != 1 || reads.Load() != 1 || revokes.Load() != 1 {
		t.Fatalf("fetch lifecycle: values=%v login=%d reads=%d revoke=%d", values, logins.Load(), reads.Load(), revokes.Load())
	}
}

func TestNoConsumerGetsNoStoreSettingsOnEitherTrigger(t *testing.T) {
	for _, trigger := range []pipeline.Trigger{pipeline.TriggerCI, pipeline.TriggerRelease} {
		r := testRequest(trigger, greedyDocument)
		r.SourceVolume = SourceVolume{ClaimName: "source", SubPath: "source", VaultCASubPath: "ca", BinarySubPath: "bin"}
		wf, err := Build(testConfig(), r)
		if err != nil {
			t.Fatal(err)
		}
		assertNoStoreSettings(t, *wf.Spec.Templates[0].Container)
		if requestNeedsSecretStore(r) {
			t.Fatal("ungranted request seeds store assets")
		}
	}
}

func assertStoreAddress(t *testing.T, container corev1.Container, address string) {
	t.Helper()
	for _, e := range container.Env {
		if e.Name == "VAULT_ADDR" {
			if e.Value != address || e.ValueFrom != nil {
				t.Errorf("address is not a literal public setting: %+v", e)
			}
			return
		}
	}
	t.Fatal("granted main lacks store address")
}

func TestGrantedStepDisablesAmbientTokens(t *testing.T) {
	r := isolationRequest(t, func(w *wfv1.Workflow) {
		leaf := &w.Spec.Templates[0]
		leaf.Container.Command = []string{OberthBinPath, "secretstore", "exec"}
		leaf.Container.Args = []string{"--path=oberth/data/release/r2-upload-token", "--", "/bin/true"}
		leaf.Sidecars = []wfv1.UserContainer{{Container: corev1.Container{Name: "sidecar", Image: leaf.Container.Image}}}
		leaf.InitContainers = []wfv1.UserContainer{{Container: corev1.Container{Name: "prepare", Image: leaf.Container.Image}}}
		w.Spec.Templates = append(w.Spec.Templates, wfv1.Template{Name: "ungranted", Container: &corev1.Container{Image: leaf.Container.Image}}, wfv1.Template{Name: "siblings", ContainerSet: &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: corev1.Container{Name: "main", Image: leaf.Container.Image}}, {Container: corev1.Container{Name: "other", Image: leaf.Container.Image}}}}})
	})
	wf, err := Build(testConfig(), r)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Spec.AutomountServiceAccountToken == nil || *wf.Spec.AutomountServiceAccountToken {
		t.Error("workflow ambient token enabled")
	}
	walkTemplates(wf, func(template *wfv1.Template) {
		if template.AutomountServiceAccountToken == nil || *template.AutomountServiceAccountToken {
			t.Errorf("%s ambient token enabled for main/sidecar/init/siblings", template.Name)
		}
		templateContainers(template, func(c *corev1.Container) {
			if template.Name != "main" || (c.Name != "" && c.Name != "main") {
				assertNoStoreSettings(t, *c)
			}
		})
	})
	main := wf.Spec.Templates[0].Container
	explicit := false
	for _, m := range main.VolumeMounts {
		explicit = explicit || m.Name == ReleaseTokenVolumeName
	}
	if !explicit {
		t.Error("granted main lost explicit token")
	}
}
