package argojob

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/oberthci/oberth/pkg/pipeline"
)

// liveOutputSymlinkDocument is the #1564 hostile step. Argo's wait container
// reads outputs.parameters valueFrom.path (and the script result) with plain
// os.ReadFile in its own mount namespace, so a symlink the step plants is
// resolved against wait's filesystem, where the executor token lives. Every
// parameter carries a default so a closed hole reads back as "absent" instead
// of failing the step.
var liveOutputSymlinkDocument = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 300
  templates:
    - name: main
      dag:
        tasks:
          - name: probe
            template: probe-burn
    - name: probe-burn
      dag:
        tasks:
          - name: leak
            template: leak
          - name: honest
            template: honest
          - name: fdprobe
            template: fdprobe
          - name: echo-honest
            depends: honest
            template: echo
            arguments:
              parameters:
                - name: result
                  value: "{{tasks.honest.outputs.result}}"
          - name: echo
            depends: leak
            template: echo
            arguments:
              parameters:
                - name: result
                  value: "{{tasks.leak.outputs.result}}"
    - name: echo
      inputs:
        parameters:
          - name: result
      container:
        image: busybox:1.37@sha256:ab33eacc8251e3807b85bb6dba570e4698c3998eca6f0fc2ccb60575a563ea74
        command: [/bin/true]
    - name: fdprobe
      script:
        image: busybox:1.37@sha256:ab33eacc8251e3807b85bb6dba570e4698c3998eca6f0fc2ccb60575a563ea74
        command: [/bin/sh]
        source: |
          mkdir -p /work/artifacts/fd
          i=3; while [ "$i" -le 64 ]; do ln -sf /proc/self/fd/$i /work/artifacts/fd/$i; i=$((i + 1)); done
      outputs:
        parameters:
` + liveFDProbeParameters + `    - name: honest
      script:
        image: busybox:1.37@sha256:ab33eacc8251e3807b85bb6dba570e4698c3998eca6f0fc2ccb60575a563ea74
        command: [/bin/sh]
        source: |
          mkdir -p /work/artifacts
          printf 'honest-parameter\n' > /work/artifacts/honest
          echo honest-result
      outputs:
        parameters:
          - name: honest
            valueFrom: {path: /work/artifacts/honest}
    - name: leak
      script:
        image: busybox:1.37@sha256:ab33eacc8251e3807b85bb6dba570e4698c3998eca6f0fc2ccb60575a563ea74
        command: [/bin/sh]
        source: |
          t=/var/run/secrets/kubernetes.io/serviceaccount/token
          mkdir -p /work/artifacts /var/run/argo/outputs/parameters/oberth-absent
          ln -sf "$t" /work/artifacts/abs || echo artifacts-symlink-refused >&2
          ln -sf ../../../../../../../../var/run/secrets/kubernetes.io/serviceaccount/token /work/artifacts/rel || true
          ln -sf "$t" /tmp/abs || echo tmp-symlink-refused >&2
          ln -sf "$t" /var/run/argo/outputs/parameters/oberth-absent/p || echo varrunargo-symlink-refused >&2
          ln -sf /var/run/secrets/kubernetes.io/serviceaccount/kubeconfig /work/artifacts/kubeconfig || true
          ln -sf /proc/self/environ /work/artifacts/environ || true
          echo marker
          rm -f /var/run/argo/ctr/main/stdout && ln -s "$t" /var/run/argo/ctr/main/stdout || echo stdout-symlink-refused >&2
      outputs:
        parameters:
          - name: artifacts-abs
            valueFrom: {path: /work/artifacts/abs, default: absent}
          - name: artifacts-rel
            valueFrom: {path: /work/artifacts/rel, default: absent}
          - name: tmp-abs
            valueFrom: {path: /tmp/abs, default: absent}
          - name: base-image
            valueFrom: {path: /oberth-absent/p, default: absent}
          - name: kubeconfig
            valueFrom: {path: /work/artifacts/kubeconfig, default: absent}
          - name: environ
            valueFrom: {path: /work/artifacts/environ, default: absent}
`

// liveFDProbeParameters reads every low descriptor of the wait process
// through /proc/self/fd/N symlinks, the route a token file held open by a
// concurrent API call would take (#1564 review).
var liveFDProbeParameters = func() string {
	var lines strings.Builder
	for fd := 3; fd <= 64; fd++ {
		fmt.Fprintf(&lines, "          - name: fd-%d\n            valueFrom: {path: /work/artifacts/fd/%d, default: absent}\n", fd, fd)
	}
	return lines.String()
}()

// TestLiveOutputPathsCannotReadTheExecutorToken submits the hostile step
// through Oberth's own Build and controller (including the real executor token
// delivery) to a pinned Argo v4.0.8 controller, then checks that no output the
// wait container captured is a usable credential. A leaked value is described
// only by length, digest prefix and the identity the API server maps it to.
func TestLiveOutputPathsCannotReadTheExecutorToken(t *testing.T) {
	cluster := requireLiveCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	cluster.ensureServiceAccounts(t, ctx)

	controller, err := NewController(cluster.workflows, cluster.kube, cluster.config())
	if err != nil {
		t.Fatalf("controller: %v", err)
	}
	controller = controller.WithSourceSeeder(NewSourceSeeder(cluster.kube, cluster.execStreamer(), cluster.config()))
	request := liveRequest("oberth-live-output-symlink", pipeline.TriggerCI, liveOutputSymlinkDocument)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		_ = controller.Cancel(cleanupCtx, request.Name, request.RunID)
	})
	if _, err := controller.Create(ctx, request); err != nil {
		t.Fatalf("create: %v", err)
	}
	log := &recordingWriter{}
	completion, err := controller.Wait(ctx, request.Name, request.RunID, log)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	t.Logf("phase=%s succeeded=%v reason=%s", completion.Phase, completion.Succeeded, completion.Reason)

	workflow, err := cluster.workflows.Get(ctx, request.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get workflow: %v", err)
	}
	captured := 0
	honest := false
	for _, node := range workflow.Status.Nodes {
		if node.TemplateName == "honest" {
			// Wait still authenticates through the kubeconfig and reports
			// ordinary outputs unchanged.
			if node.Outputs == nil || node.Outputs.Result == nil || *node.Outputs.Result != "honest-result" ||
				len(node.Outputs.Parameters) != 1 || node.Outputs.Parameters[0].Value == nil ||
				node.Outputs.Parameters[0].Value.String() != "honest-parameter" {
				t.Errorf("honest step outputs = %+v (phase=%s message=%q)", node.Outputs, node.Phase, node.Message)
			}
			honest = true
		}
		if node.TemplateName != "leak" && node.TemplateName != "fdprobe" {
			continue
		}
		t.Logf("leak node phase=%s message=%q", node.Phase, node.Message)
		if node.Outputs == nil {
			continue
		}
		values := map[string]string{}
		for _, parameter := range node.Outputs.Parameters {
			if parameter.Value != nil {
				values["parameter "+parameter.Name] = parameter.Value.String()
			}
		}
		if node.Outputs.Result != nil {
			values["script result"] = *node.Outputs.Result
		}
		for name, value := range values {
			captured++
			if leak := describeCredential(ctx, cluster.restConfig, value); leak != "" {
				t.Errorf("LEAK via %s: %s", name, leak)
			} else {
				t.Logf("no credential via %s (%d bytes)", name, len(value))
			}
		}
	}
	if !honest {
		t.Error("the honest step did not run")
	}
	if captured == 0 {
		t.Fatalf("the hostile step reported no outputs; log:\n%s", truncate(log.String(), 4000))
	}
}

// jwtPattern matches a compact JWS anywhere in a captured output.
var jwtPattern = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)

// describeCredential returns "" unless value contains a ServiceAccount token,
// in which case it describes it without including it: length, digest prefix,
// subject, and what the API server says about it now.
func describeCredential(ctx context.Context, config *rest.Config, value string) string {
	token := jwtPattern.FindString(value)
	if token == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(token))
	description := "len=" + strconv.Itoa(len(token)) + " sha256=" + hex.EncodeToString(digest[:])[:12]
	if claims, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[1]); err == nil {
		var decoded struct {
			Subject string `json:"sub"`
		}
		if json.Unmarshal(claims, &decoded) == nil {
			description += " sub=" + decoded.Subject
		}
	}
	anonymous := rest.AnonymousClientConfig(config)
	anonymous.BearerToken = token
	client, err := kubernetes.NewForConfig(anonymous)
	if err != nil {
		return description + " (client: " + err.Error() + ")"
	}
	review, err := client.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		return description + " (API rejected it: " + err.Error() + ")"
	}
	return description + " authenticates as " + review.Status.UserInfo.Username
}
