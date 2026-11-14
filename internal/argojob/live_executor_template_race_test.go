package argojob

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	wfclientset "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/oberthci/oberth/pkg/pipeline"
)

// This is a frequency measurement, not an assertion that a race must occur.
// Run before and after separately against the same disposable kind cluster.
func TestLiveExecutorTemplateRaceMeasurement(t *testing.T) {
	cluster := requireLiveCluster(t)
	// Eight concurrent test observers should not share client-go's five-QPS
	// default. This changes only the measurement client, never server policy.
	config := rest.CopyConfig(cluster.restConfig)
	config.QPS, config.Burst = 40, 80
	kube, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	argo, err := wfclientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	cluster.kube, cluster.restConfig = kube, config
	cluster.workflows = argo.ArgoprojV1alpha1().Workflows(cluster.namespace)
	cluster.ensureServiceAccounts(t, t.Context())
	nodes, err := cluster.kube.CoreV1().Nodes().List(t.Context(), metav1.ListOptions{})
	if err != nil || len(nodes.Items) != 1 {
		t.Fatalf("measurement needs exactly one node: nodes=%d err=%v", len(nodes.Items), err)
	}
	installLiveTaskResultPolicy(t, cluster)
	for _, phase := range []string{"before", "after"} {
		t.Run(phase, func(t *testing.T) {
			for _, stressed := range []bool{false, true} {
				name := "idle"
				if stressed {
					name = "stressed"
				}
				t.Run(name, func(t *testing.T) { measureLiveTemplateRace(t, cluster, phase+"-"+name, stressed) })
			}
		})
	}
}

type templateRaceObservation struct {
	Run          string `json:"run"`
	Marker       bool   `json:"wait_loaded_marker"`
	Original     bool   `json:"wait_loaded_original"`
	Swap         bool   `json:"main_swapped_template"`
	Phase        string `json:"workflow_phase"`
	Error        string `json:"error,omitempty"`
	LoadSameNode bool   `json:"load_same_node,omitempty"`
	LoadOverlap  bool   `json:"load_overlap,omitempty"`
}

func measureLiveTemplateRace(t *testing.T, cluster liveCluster, phase string, stressed bool) {
	t.Helper()
	var mutex sync.Mutex
	runs := 300
	if stressed {
		// One contention Workflow at a time keeps the shared host within its load budget.
		runs = 50
	}
	observations := make([]templateRaceObservation, 0, runs)
	prefix := fmt.Sprintf("r7-race-%s-%d", phase, time.Now().Unix())
	t.Run("runs", func(t *testing.T) {
		for i := range runs {
			t.Run(fmt.Sprintf("%03d", i), func(t *testing.T) {
				if !stressed {
					t.Parallel()
				}
				observation := observeLiveTemplateRace(t, cluster, fmt.Sprintf("%s-%03d", prefix, i), stressed)
				mutex.Lock()
				observations = append(observations, observation)
				done := len(observations)
				mutex.Unlock()
				if done%25 == 0 {
					t.Logf("%s measurement: %d/%d complete", phase, done, runs)
				}
				if observation.Error != "" {
					t.Logf("measurement observation: %s", observation.Error)
				}
			})
		}
	})
	markers, originals, swaps := 0, 0, 0
	for _, v := range observations {
		if v.Marker {
			markers++
		}
		if v.Original {
			originals++
		}
		if v.Swap {
			swaps++
		}
	}
	summary := struct {
		Phase        string                    `json:"phase"`
		Runs         int                       `json:"runs"`
		Marker       int                       `json:"wait_loaded_marker"`
		Original     int                       `json:"wait_loaded_original"`
		Swaps        int                       `json:"main_swapped_template"`
		Observations []templateRaceObservation `json:"observations"`
	}{phase, len(observations), markers, originals, swaps, observations}
	body, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(t.ArtifactDir(), "measurement.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: N=%d; main swaps=%d; wait marker=%d; wait original=%d", phase, len(observations), swaps, markers, originals)
}

func observeLiveTemplateRace(t *testing.T, cluster liveCluster, name string, stressed bool) (observation templateRaceObservation) {
	t.Helper()
	observation.Run = name
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	source := templateRaceSource(stressed)
	request := liveRequest(name, pipeline.TriggerCI, source)
	workflow, err := Build(cluster.config(), request)
	if err != nil {
		observation.Error = err.Error()
		return
	}
	controller, err := NewController(cluster.workflows, cluster.kube, cluster.config())
	if err != nil {
		observation.Error = err.Error()
		return
	}
	controller = controller.WithSourceSeeder(NewSourceSeeder(cluster.kube, cluster.execStreamer(), cluster.config()))
	current, err := cluster.workflows.Create(ctx, workflow, metav1.CreateOptions{})
	if err != nil {
		observation.Error = err.Error()
		return
	}
	delivered := map[types.UID]bool{}
	for !current.Status.Phase.Completed() {
		if err = controller.deliverExecutorTokens(ctx, current, delivered); err != nil {
			observation.Error = err.Error()
			return
		}
		select {
		case <-ctx.Done():
			observation.Error = ctx.Err().Error()
			return
		case <-time.After(time.Second):
		}
		current, err = cluster.workflows.Get(ctx, request.Name, metav1.GetOptions{})
		if err != nil {
			observation.Error = err.Error()
			return
		}
	}
	observation.Phase = string(current.Status.Phase)
	pods, err := cluster.kube.CoreV1().Pods(cluster.namespace).List(ctx, metav1.ListOptions{LabelSelector: "workflows.argoproj.io/workflow=" + name})
	if err != nil {
		observation.Error = err.Error()
		return
	}
	var loadPod *corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if current.Status.Nodes[pod.Annotations["workflows.argoproj.io/node-id"]].TemplateName == "load" {
			loadPod = pod
			body, err := json.MarshalIndent(pod, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(t.ArtifactDir(), "load-pod.json"), body, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, pod := range pods.Items {
		if current.Status.Nodes[pod.Annotations["workflows.argoproj.io/node-id"]].TemplateName != "main" {
			continue
		}
		if pod.Labels["oberth.ci/role"] == "executor-identity" {
			continue
		}
		body, err := json.MarshalIndent(pod, "", "  ")
		if err != nil {
			observation.Error = err.Error()
			return
		}
		if err = os.WriteFile(filepath.Join(t.ArtifactDir(), "pod.json"), body, 0o600); err != nil {
			observation.Error = err.Error()
			return
		}
		if stressed {
			observation.LoadSameNode = loadPod != nil && loadPod.Spec.NodeName == pod.Spec.NodeName && pod.Spec.NodeName != ""
			if loadPod != nil {
				for _, load := range loadPod.Status.ContainerStatuses {
					for _, target := range pod.Status.ContainerStatuses {
						if load.Name == "main" && target.Name == "main" && load.State.Terminated != nil && target.State.Terminated != nil {
							observation.LoadOverlap = load.State.Terminated.StartedAt.Before(&target.State.Terminated.FinishedAt) && target.State.Terminated.StartedAt.Before(&load.State.Terminated.FinishedAt)
						}
					}
				}
			}
		}
		for _, container := range []string{"main", "wait"} {
			logs, err := templateRaceLogs(ctx, cluster, pod.Name, container)
			if err != nil {
				observation.Error = err.Error()
				return
			}
			if err = os.WriteFile(filepath.Join(t.ArtifactDir(), container+".log"), logs, 0o600); err != nil {
				observation.Error = err.Error()
				return
			}
			if container == "main" {
				observation.Swap = strings.Contains(string(logs), "r6-template-swap-complete")
				continue
			}
			for _, line := range strings.Split(string(logs), "\n") {
				if !strings.Contains(line, "Executor initialized") {
					continue
				}
				observation.Marker = strings.Contains(line, "templateName=r6-marker")
				observation.Original = strings.Contains(line, "templateName=main")
			}
		}
	}
	if !observation.Marker && !observation.Original {
		observation.Error = "wait initialization marker missing"
	}
	if !observation.Swap {
		observation.Error = "main did not complete template swap"
	}
	return
}

func templateRaceSource(stressed bool) string {
	source := `apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 120
  templates:
  - name: main
    container:
      image: busybox:1.37@sha256:ab33eacc8251e3807b85bb6dba570e4698c3998eca6f0fc2ccb60575a563ea74
      workingDir: /tmp
      command: [sh, -c]
      args:
      - |
        sed 's/"name":"main"/"name":"r6-marker"/' /var/run/argo/template > /var/run/argo/r6-swap
        mv /var/run/argo/r6-swap /var/run/argo/template
        grep -q '"name":"r6-marker"' /var/run/argo/template || exit 70
        echo r6-template-swap-complete
`
	if stressed {
		source = strings.Replace(source, "entrypoint: main", "entrypoint: parallel", 1)
		source += `
  - name: parallel
    steps:
    - - name: load
        template: load
      - name: race
        template: race
  - name: race
    steps:
    - - name: warmup
        template: warmup
    - - name: swap
        template: main
  - name: warmup
    container:
      image: busybox:1.37@sha256:ab33eacc8251e3807b85bb6dba570e4698c3998eca6f0fc2ccb60575a563ea74
      command: [sleep, '2']
  - name: load
    activeDeadlineSeconds: 30
    container:
      image: busybox:1.37@sha256:ab33eacc8251e3807b85bb6dba570e4698c3998eca6f0fc2ccb60575a563ea74
      command: [sh, -c]
      args: ["timeout 12 sh -c 'while :; do :; done' & timeout 12 sh -c 'while :; do :; done'; wait; exit 0"]
      resources:
        requests:
          cpu: 100m
        limits:
          cpu: '2'
`
	}
	return source
}

func templateRaceLogs(ctx context.Context, cluster liveCluster, pod, container string) ([]byte, error) {
	marker := "Executor initialized"
	if container == "main" {
		marker = "r6-template-swap-complete"
	}
	var body []byte
	for attempt := range 5 {
		var err error
		body, err = cluster.kube.CoreV1().Pods(cluster.namespace).GetLogs(pod, &corev1.PodLogOptions{Container: container}).DoRaw(ctx)
		if err != nil || strings.Contains(string(body), marker) {
			return body, err
		}
		if attempt < 4 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
	return body, nil
}
