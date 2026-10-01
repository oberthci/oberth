package argojob

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// trivyWorkflow builds a minimal workflow with a DAG entrypoint and the
// given tasks. When withWorkVCT is true, a "work" volumeClaimTemplate is
// declared (simulating the standard workspace).
func trivyWorkflow(withWorkVCT bool, tasks ...wfv1.DAGTask) *wfv1.Workflow {
	w := &wfv1.Workflow{
		Spec: wfv1.WorkflowSpec{
			Entrypoint: "main",
			Templates: []wfv1.Template{
				{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: tasks}},
			},
		},
	}
	if withWorkVCT {
		w.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{
			{ObjectMeta: trivyObjectMeta("work"), Spec: corev1.PersistentVolumeClaimSpec{
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			}},
		}
	}
	return w
}

func trivyObjectMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name}
}

func trivyTemplate(name string, cacheDirArg string, workspaceMountsNone bool) wfv1.Template {
	args := []string{"fs", "--cache-dir", cacheDirArg, "."}
	t := wfv1.Template{
		Name: name,
		Container: &corev1.Container{
			Image:   "mirror.gcr.io/aquasec/trivy:0.73.0",
			Command: []string{"trivy"},
			Args:    args,
		},
	}
	if workspaceMountsNone {
		t.Metadata = wfv1.Metadata{
			Annotations: map[string]string{
				argoworkflow.WorkspaceMountsAnnotation: "none",
			},
		}
	}
	return t
}

func trivyInlineDAGTask(name string, inline wfv1.Template, depends string) wfv1.DAGTask {
	return wfv1.DAGTask{Name: name, Inline: &inline, Depends: depends}
}

// TestTrivyGuardRejectsUnorderedSharedCache verifies that two unordered Trivy
// leaves sharing the server-injected workspace trivy-cache mount are rejected.
// This is the s3-proxy red shape from issue #655.
func TestTrivyGuardRejectsUnorderedSharedCache(t *testing.T) {
	security := trivyTemplate("security", WorkspaceTrivyCacheMountPath, false)
	scanServer := trivyTemplate("scan-test-s3-server", WorkspaceTrivyCacheMountPath, false)

	w := trivyWorkflow(true,
		trivyInlineDAGTask("security", security, ""),
		trivyInlineDAGTask("scan-test-s3-server", scanServer, "install-test-s3-server"),
		wfv1.DAGTask{Name: "install-test-s3-server"},
	)

	err := trivyGuard(w)
	if err == nil {
		t.Fatal("expected trivyGuard to reject unordered Trivy leaves with shared cache")
	}
	if !strings.Contains(err.Error(), "security") || !strings.Contains(err.Error(), "scan-test-s3-server") {
		t.Fatalf("error should name both tasks: %v", err)
	}
	if !strings.Contains(err.Error(), "#655") {
		t.Fatalf("error should reference issue #655: %v", err)
	}
}

// TestTrivyGuardAcceptsOrderedPair verifies that two Trivy leaves with an
// ordering edge between them are accepted.
func TestTrivyGuardAcceptsOrderedPair(t *testing.T) {
	security := trivyTemplate("security", WorkspaceTrivyCacheMountPath, false)
	scanServer := trivyTemplate("scan-test-s3-server", WorkspaceTrivyCacheMountPath, false)

	w := trivyWorkflow(true,
		trivyInlineDAGTask("security", security, ""),
		// scan-test-s3-server depends on security -> ordered pair
		trivyInlineDAGTask("scan-test-s3-server", scanServer, "security"),
	)

	if err := trivyGuard(w); err != nil {
		t.Fatalf("ordered Trivy pair should be accepted: %v", err)
	}
}

// TestTrivyGuardAcceptsPrivateCache verifies that two unordered Trivy leaves
// with private caches (workspace-mounts: none) are accepted. This is the
// acme-cli shape.
func TestTrivyGuardAcceptsPrivateCache(t *testing.T) {
	security := trivyTemplate("security", WorkspaceTrivyCacheMountPath, true)
	scanSystemd := trivyTemplate("scan-systemd-tools", WorkspaceTrivyCacheMountPath, true)

	w := trivyWorkflow(true,
		trivyInlineDAGTask("security", security, "install-golangci-lint"),
		trivyInlineDAGTask("scan-systemd-tools", scanSystemd, "install-systemd-tools"),
		wfv1.DAGTask{Name: "install-golangci-lint"},
		wfv1.DAGTask{Name: "install-systemd-tools"},
	)

	if err := trivyGuard(w); err != nil {
		t.Fatalf("private-cache Trivy leaves should be accepted: %v", err)
	}
}

// TestTrivyGuardAcceptsNoWorkVCT verifies that without a work VCT, the
// trivy cache path is not shared (even if workspace-mounts is not none).
func TestTrivyGuardAcceptsNoWorkVCT(t *testing.T) {
	security := trivyTemplate("security", WorkspaceTrivyCacheMountPath, false)
	scanServer := trivyTemplate("scan-test", WorkspaceTrivyCacheMountPath, false)

	w := trivyWorkflow(false, // no work VCT
		trivyInlineDAGTask("security", security, ""),
		trivyInlineDAGTask("scan-test", scanServer, ""),
	)

	if err := trivyGuard(w); err != nil {
		t.Fatalf("no work VCT means cache is private: %v", err)
	}
}

// TestTrivyGuardAcceptsSingleTrivy verifies that a single Trivy leaf is
// accepted (no pair to conflict with).
func TestTrivyGuardAcceptsSingleTrivy(t *testing.T) {
	security := trivyTemplate("security", WorkspaceTrivyCacheMountPath, false)

	w := trivyWorkflow(true,
		trivyInlineDAGTask("security", security, ""),
	)

	if err := trivyGuard(w); err != nil {
		t.Fatalf("single Trivy leaf should be accepted: %v", err)
	}
}

// TestTrivyGuardAcceptsTransitiveOrder verifies that a transitive ordering
// edge (a->b->c) makes a and c ordered.
func TestTrivyGuardAcceptsTransitiveOrder(t *testing.T) {
	security := trivyTemplate("security", WorkspaceTrivyCacheMountPath, false)
	scanServer := trivyTemplate("scan-test", WorkspaceTrivyCacheMountPath, false)

	w := trivyWorkflow(true,
		trivyInlineDAGTask("security", security, ""),
		wfv1.DAGTask{Name: "build", Depends: "security"},
		trivyInlineDAGTask("scan-test", scanServer, "build"),
	)

	if err := trivyGuard(w); err != nil {
		t.Fatalf("transitively ordered Trivy leaves should be accepted: %v", err)
	}
}

// TestTrivyGuardRejectsSharedVCTMount verifies that two Trivy leaves sharing
// a repo-declared VCT mount (not the server-injected one) are rejected when
// unordered. This covers a pipeline that declares workspace-mounts: none but
// explicitly mounts the work VCT at the trivy cache path.
func TestTrivyGuardRejectsSharedVCTMount(t *testing.T) {
	security := trivyTemplate("security", "/my/trivy-cache", true) // workspace-mounts: none
	security.Container.VolumeMounts = []corev1.VolumeMount{
		{Name: "work", MountPath: "/my/trivy-cache", SubPath: "trivy"},
	}
	scan := trivyTemplate("scan", "/my/trivy-cache", true) // workspace-mounts: none
	scan.Container.VolumeMounts = []corev1.VolumeMount{
		{Name: "work", MountPath: "/my/trivy-cache", SubPath: "trivy"},
	}

	w := trivyWorkflow(true,
		trivyInlineDAGTask("security", security, ""),
		trivyInlineDAGTask("scan", scan, ""),
	)

	err := trivyGuard(w)
	if err == nil {
		t.Fatal("expected trivyGuard to reject unordered Trivy leaves with explicit shared VCT mount")
	}
}

// TestTrivyGuardParseDependencies verifies both enhanced depends and legacy
// dependencies parsing.
func TestTrivyGuardParseDependencies(t *testing.T) {
	tests := []struct {
		name     string
		task     wfv1.DAGTask
		expected []string
	}{
		{
			name:     "enhanced depends with &&",
			task:     wfv1.DAGTask{Depends: "install-test-s3-server && security"},
			expected: []string{"install-test-s3-server", "security"},
		},
		{
			name:     "legacy dependencies",
			task:     wfv1.DAGTask{Dependencies: []string{"setup", "lint"}},
			expected: []string{"setup", "lint"},
		},
		{
			name:     "enhanced depends with status suffix",
			task:     wfv1.DAGTask{Depends: "build.Succeeded && test.Succeeded"},
			expected: []string{"build", "test"},
		},
		{
			name:     "empty",
			task:     wfv1.DAGTask{},
			expected: nil,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := parseDependencies(test.task)
			if len(got) != len(test.expected) {
				t.Fatalf("expected %v, got %v", test.expected, got)
			}
			for i := range got {
				if got[i] != test.expected[i] {
					t.Fatalf("expected %v, got %v", test.expected, got)
				}
			}
		})
	}
}

// TestTrivyGuardCacheDirExtraction verifies --cache-dir parsing from various
// command/args shapes.
func TestTrivyGuardCacheDirExtraction(t *testing.T) {
	tests := []struct {
		name     string
		command  []string
		args     []string
		expected string
	}{
		{
			name:     "separate flag and value",
			command:  []string{"trivy"},
			args:     []string{"fs", "--cache-dir", "/tmp/oberth-trivy", "."},
			expected: "/tmp/oberth-trivy",
		},
		{
			name:     "equals form",
			command:  []string{"trivy"},
			args:     []string{"fs", "--cache-dir=/custom/cache", "."},
			expected: "/custom/cache",
		},
		{
			name:     "no cache dir",
			command:  []string{"trivy"},
			args:     []string{"fs", "."},
			expected: "",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			template := &wfv1.Template{
				Container: &corev1.Container{Command: test.command, Args: test.args},
			}
			got := extractTrivyCacheDir(template)
			if got != test.expected {
				t.Fatalf("expected %q, got %q", test.expected, got)
			}
		})
	}
}

// TestTrivyGuardByNameTemplate verifies that by-name template references
// (not just inline) are resolved correctly.
func TestTrivyGuardByNameTemplate(t *testing.T) {
	securityTmpl := trivyTemplate("security", WorkspaceTrivyCacheMountPath, false)
	scanTmpl := trivyTemplate("scan", WorkspaceTrivyCacheMountPath, false)

	w := &wfv1.Workflow{
		Spec: wfv1.WorkflowSpec{
			Entrypoint: "main",
			Templates: []wfv1.Template{
				{Name: "main", DAG: &wfv1.DAGTemplate{
					Tasks: []wfv1.DAGTask{
						{Name: "security", Template: "security"},
						{Name: "scan", Template: "scan"},
					},
				}},
				securityTmpl,
				scanTmpl,
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{ObjectMeta: trivyObjectMeta("work"), Spec: corev1.PersistentVolumeClaimSpec{
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
					},
				}},
			},
		},
	}

	err := trivyGuard(w)
	if err == nil {
		t.Fatal("expected rejection for unordered by-name Trivy templates")
	}
}
