package argoworkflow

import (
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

// sensitiveTypes are the Kubernetes and Argo types that, if reachable from a
// repository-authored document, hand a pipeline something Oberth did not grant
// it: a Secret or ConfigMap value, the node filesystem or network, a
// scheduling decision, or a privilege boundary.
//
// The map value records why each one matters, so a reviewer reading a failing
// diff knows what a newly reachable path actually costs.
var sensitiveTypes = map[string]string{
	"SecretKeySelector":                 "reads a Kubernetes Secret value",
	"ConfigMapKeySelector":              "reads a Kubernetes ConfigMap value",
	"SecretEnvSource":                   "reads a whole Kubernetes Secret into the environment",
	"ConfigMapEnvSource":                "reads a whole Kubernetes ConfigMap into the environment",
	"SecretVolumeSource":                "projects a Kubernetes Secret into the Pod filesystem",
	"ConfigMapVolumeSource":             "projects a Kubernetes ConfigMap into the Pod filesystem",
	"ProjectedVolumeSource":             "projects ServiceAccount tokens or Secrets into the Pod",
	"CSIVolumeSource":                   "mounts a driver-provided volume, including secret stores",
	"HostPathVolumeSource":              "mounts the node filesystem",
	"PersistentVolumeClaimVolumeSource": "mounts an existing claim, possibly another tenant's",
	"LocalObjectReference":              "names a Secret or ConfigMap by reference",
	"Affinity":                          "chooses which node the run lands on",
	"Toleration":                        "schedules onto tainted nodes",
	"HostAlias":                         "rewrites name resolution inside the Pod",
	"PodDNSConfig":                      "rewrites DNS resolution inside the Pod",
	"Capabilities":                      "adds Linux capabilities",
	"SELinuxOptions":                    "overrides the SELinux label",
	"SeccompProfile":                    "overrides the seccomp profile",
	"AppArmorProfile":                   "overrides the AppArmor profile",
	"Sysctl":                            "sets kernel parameters",

	// SecurityContext and PodSecurityContext are deliberately ABSENT from this
	// list, and that absence is load-bearing.
	//
	// Listing them made the walk stop at the struct and report the whole
	// security surface as one path, which "SecurityContext" then closed --
	// so the audit reported the area as covered while seccompProfile:
	// Unconfined and runAsUser sailed through a gate that only checked
	// privileged, allowPrivilegeEscalation, and capabilities.add. A leaf that
	// terminates the walk hides every field beneath it, which is exactly the
	// failure mode this test exists to prevent.
	//
	// Omitted, the walk descends into them and enumerates SeccompProfile,
	// AppArmorProfile, Capabilities, SELinuxOptions and the rest individually,
	// each of which must then be closed by a named root.
}

// TestSensitiveSurfaceIsKnown enumerates every path by which a
// repository-authored WorkflowSpec can reach a credential, the node, or a
// privilege boundary, and fails when that set changes.
//
// It generalizes TestIdentityFieldPathsAreKnown from identity alone to the
// whole attack surface. The identity normalizer is exhaustive by construction
// (it matches on field name), but the Admit gate is a deny list of named
// constructs, and a deny list silently stops being complete when the schema
// grows. This test makes an Argo upgrade that opens a new door a failing build
// with a named path, rather than a gap nobody notices until it is used.
//
// A failure here is not automatically a vulnerability: many of these paths are
// already closed because Admit refuses the construct that contains them (for
// example every artifact path is unreachable because artifacts are refused
// outright). It means a human must look at the new path and decide.
// The pinned surface lives in a golden file rather than Go source: it is 600
// paths long, and its whole value is being readable as a diff when an Argo
// upgrade changes it. Regenerate with:
//
//	OBERTH_UPDATE_SENSITIVE_SURFACE=1 go test ./pkg/argoworkflow/ -run TestSensitiveSurfaceIsKnown
//
// and read the diff before committing it.
const sensitiveSurfaceGolden = "testdata/sensitive-surface.txt"

func TestSensitiveSurfaceIsKnown(t *testing.T) {
	found := sensitiveFieldPaths(reflect.TypeOf(wfv1.WorkflowSpec{}), "WorkflowSpec", map[reflect.Type]bool{}, 0)
	slices.Sort(found)
	found = slices.Compact(found)

	if os.Getenv("OBERTH_UPDATE_SENSITIVE_SURFACE") != "" {
		if err := os.WriteFile(sensitiveSurfaceGolden, []byte(strings.Join(found, "\n")+"\n"), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Fatalf("regenerated %s with %d paths; review the diff and re-run without the environment variable",
			sensitiveSurfaceGolden, len(found))
	}

	raw, err := os.ReadFile(sensitiveSurfaceGolden)
	if err != nil {
		t.Fatalf("read %s: %v", sensitiveSurfaceGolden, err)
	}
	known := strings.Split(strings.TrimSpace(string(raw)), "\n")
	slices.Sort(known)

	for _, path := range found {
		if !slices.Contains(known, path) {
			t.Errorf("upstream schema exposes a sensitive path Admit has not been reviewed against:\n"+
				"  %s\n"+
				"  (%s)\n"+
				"  confirm Admit refuses it, then regenerate %s", path, sensitiveReason(path), sensitiveSurfaceGolden)
		}
	}
	for _, path := range known {
		if !slices.Contains(found, path) {
			t.Errorf("sensitive path %s no longer exists upstream; regenerate %s", path, sensitiveSurfaceGolden)
		}
	}
}

// surfaceRoot documents how one family of sensitive paths is closed, and
// carries a document that must actually be refused for the claim to hold.
type surfaceRoot struct {
	// why explains the closure in review terms.
	why string
	// document is a Workflow that reaches this root. Admit must refuse it.
	// Empty means the root is closed by construction rather than by a check
	// (the reason must say how).
	document string
}

// surfaceRoots maps a type-graph field name to the construct that makes every
// sensitive path beneath it unreachable.
//
// Writing this down was worth more than the golden file: it revealed that two
// roots this map originally claimed -- spec.dnsConfig and parameter
// valueFrom.configMapKeyRef -- had no implementation behind them at all. A
// claim with an executable proof attached cannot drift like that.
func surfaceRoots() map[string]surfaceRoot {
	withSpec := func(insert string) string {
		return strings.Replace(admissibleDocument, "  entrypoint: main\n", "  entrypoint: main\n"+insert, 1)
	}
	withTemplate := func(insert string) string {
		return strings.Replace(admissibleDocument, "    - name: main\n", "    - name: main\n"+insert, 1)
	}
	withContainer := func(insert string) string {
		return strings.Replace(admissibleDocument, "        command: [/bin/true]\n", "        command: [/bin/true]\n"+insert, 1)
	}
	return map[string]surfaceRoot{
		"Artifacts": {"template and argument artifacts are refused",
			withTemplate("      inputs:\n        artifacts:\n          - name: in\n            path: /in\n")},
		"ArtifactRepositoryRef": {"spec.artifactRepositoryRef is refused",
			withSpec("  artifactRepositoryRef:\n    configMap: repos\n    key: default\n")},
		"ArchiveLocation": {"template.archiveLocation is refused",
			withTemplate("      archiveLocation:\n        archiveLogs: true\n")},
		"Data": {"template.data is refused",
			withTemplate("      data:\n        source:\n          artifactPaths:\n            name: in\n            s3:\n              bucket: b\n        transformation: []\n")},
		"Resource": {"template.resource is refused",
			withTemplate("      resource:\n        action: create\n        manifest: 'kind: Pod'\n")},
		"Plugin": {"template.plugin is refused",
			withTemplate("      plugin:\n        anything: {}\n")},
		"HTTP": {"template.http is refused",
			withTemplate("      http:\n        url: https://example.invalid\n")},
		"ImagePullSecrets": {"spec.imagePullSecrets is refused",
			withSpec("  imagePullSecrets:\n    - name: registry\n")},
		"Affinity": {"node targeting is refused",
			withSpec("  affinity:\n    nodeAffinity:\n      requiredDuringSchedulingIgnoredDuringExecution:\n        nodeSelectorTerms: []\n")},
		"Tolerations": {"node targeting is refused",
			withSpec("  tolerations:\n    - operator: Exists\n")},
		"NodeSelector": {"node targeting is refused",
			withSpec("  nodeSelector:\n    kubernetes.io/hostname: tuxbox\n")},
		"HostAliases": {"host aliases are refused",
			withSpec("  hostAliases:\n    - ip: 127.0.0.1\n      hostnames: [vault]\n")},
		"DNSConfig": {"spec.dnsConfig and spec.dnsPolicy are refused",
			withSpec("  dnsConfig:\n    nameservers: [10.0.0.1]\n")},
		"Volumes": {"only emptyDir volumes are admitted",
			withSpec("  volumes:\n    - name: creds\n      secret:\n        secretName: release\n")},
		"EnvFrom": {"container envFrom Secret and ConfigMap reads are refused",
			withContainer("        envFrom:\n          - secretRef:\n              name: release\n")},
		"Env": {"container env valueFrom Secret and ConfigMap reads are refused",
			withContainer("        env:\n          - name: T\n            valueFrom:\n              secretKeyRef:\n                name: release\n                key: token\n")},
		"ValueFrom": {"parameter valueFrom ConfigMap reads are refused",
			withSpec("  arguments:\n    parameters:\n      - name: p\n        valueFrom:\n          configMapKeyRef:\n            name: c\n            key: k\n")},
		"Memoize": {"template.memoize is refused",
			withTemplate("      memoize:\n        key: k\n        maxAge: 1h\n        cache:\n          configMap:\n            name: c\n            key: k\n")},
		"SecurityContext": {"the whole container security context is refused; the engine forces the baseline",
			withContainer("        securityContext:\n          privileged: true\n")},
		"PodSecurityContext": {"the whole Pod security context is refused; the engine forces the baseline",
			withSpec("  securityContext:\n    runAsUser: 0\n")},
		"PodMetadata": {"repository-authored Pod metadata is refused",
			withSpec("  podMetadata:\n    annotations:\n      container.apparmor.security.beta.kubernetes.io/main: unconfined\n")},
		"Metadata": {"repository-authored Pod metadata is refused",
			withTemplate("      metadata:\n        annotations:\n          container.apparmor.security.beta.kubernetes.io/main: unconfined\n")},

		// Closed by construction rather than by a check.
		"Metrics":         {"Prometheus metric definitions carry no credential selector; the type-graph hit is the shared SecurityContext reachable through them", ""},
		"Synchronization": {"a semaphore's ConfigMap key holds an integer concurrency limit that is never surfaced to the pipeline; mutexes and semaphores are required for chart-index compare-and-swap", ""},
		"Sidecar":         {"executor plugin sidecars are not reachable from WorkflowSpec in this schema version", ""},
	}
}

// TestSensitiveSurfaceIsClosedAtItsRoots proves every sensitive path is
// unreachable because Admit refuses a construct on the way to it -- and that
// each claimed refusal is real, by submitting a document that reaches it.
func TestSensitiveSurfaceIsClosedAtItsRoots(t *testing.T) {
	roots := surfaceRoots()
	raw, err := os.ReadFile(sensitiveSurfaceGolden)
	if err != nil {
		t.Fatalf("read %s: %v", sensitiveSurfaceGolden, err)
	}
	for _, path := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if !closedAtSomeRoot(path, roots) {
			t.Errorf("no documented root closes %s\n"+
				"  every sensitive path must be unreachable because Admit refuses a construct on the way to it", path)
		}
	}

	for name, root := range roots {
		if root.document == "" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			workflow, err := Decode([]byte(root.document))
			if err != nil {
				// A document the decoder itself rejects is closed even earlier.
				return
			}
			if err := Admit(workflow, Policy{}); err == nil {
				t.Fatalf("%s claims %q, but Admit accepted a document reaching it", name, root.why)
			}
		})
	}
}

func closedAtSomeRoot(path string, roots map[string]surfaceRoot) bool {
	for _, segment := range strings.Split(strings.SplitN(path, ":", 2)[0], ".") {
		if _, closed := roots[segment]; closed {
			return true
		}
	}
	return false
}

func sensitiveReason(path string) string {
	segments := strings.Split(path, ":")
	if len(segments) < 2 {
		return "unknown"
	}
	return sensitiveTypes[segments[len(segments)-1]]
}

// sensitiveFieldPaths walks the type graph and reports "<field path>:<type>"
// for every field whose type is one of the sensitive types above.
func sensitiveFieldPaths(walked reflect.Type, path string, visiting map[reflect.Type]bool, depth int) []string {
	if depth > 10 {
		return nil
	}
	for walked.Kind() == reflect.Pointer || walked.Kind() == reflect.Slice ||
		walked.Kind() == reflect.Array || walked.Kind() == reflect.Map {
		walked = walked.Elem()
	}
	if walked.Kind() != reflect.Struct || visiting[walked] {
		return nil
	}
	visiting[walked] = true
	defer delete(visiting, walked)
	var paths []string
	for index := range walked.NumField() {
		field := walked.Field(index)
		if !field.IsExported() {
			continue
		}
		next := path + "." + field.Name
		leaf := field.Type
		for leaf.Kind() == reflect.Pointer || leaf.Kind() == reflect.Slice ||
			leaf.Kind() == reflect.Array || leaf.Kind() == reflect.Map {
			leaf = leaf.Elem()
		}
		if _, sensitive := sensitiveTypes[leaf.Name()]; sensitive {
			paths = append(paths, next+":"+leaf.Name())
			continue
		}
		paths = append(paths, sensitiveFieldPaths(field.Type, next, visiting, depth+1)...)
	}
	return paths
}
