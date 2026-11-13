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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/pkg/pipeline"
)

// This is a frequency measurement, not an assertion that a race must occur.
// Run before and after separately against the same disposable kind cluster.
func TestLiveExecutorTemplateRaceMeasurement(t *testing.T) {
	cluster := requireLiveCluster(t)
	cluster.ensureServiceAccounts(t, t.Context())
	for _, phase := range []string{"before", "after"} {
		t.Run(phase, func(t *testing.T) {
			if phase == "after" {
				installLiveTaskResultPolicy(t, cluster)
			}
			measureLiveTemplateRace(t, cluster, phase)
		})
	}
}

type templateRaceObservation struct {
	Run      string `json:"run"`
	Marker   bool   `json:"wait_loaded_marker"`
	Original bool   `json:"wait_loaded_original"`
	Swap     bool   `json:"main_swapped_template"`
	Phase    string `json:"workflow_phase"`
	Error    string `json:"error,omitempty"`
}

func measureLiveTemplateRace(t *testing.T, cluster liveCluster, phase string) {
	t.Helper()
	var mutex sync.Mutex
	observations := make([]templateRaceObservation, 0, 300)
	t.Run("runs", func(t *testing.T) {
		for i := range 300 {
			t.Run(fmt.Sprintf("%03d", i), func(t *testing.T) {
				t.Parallel()
				observation := observeLiveTemplateRace(t, cluster, fmt.Sprintf("r6-race-%s-%03d", phase, i))
				mutex.Lock()
				observations = append(observations, observation)
				done := len(observations)
				mutex.Unlock()
				if done%25 == 0 {
					t.Logf("%s measurement: %d/300 complete", phase, done)
				}
				if observation.Error != "" {
					t.Errorf("measurement incomplete: %s", observation.Error)
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

func observeLiveTemplateRace(t *testing.T, cluster liveCluster, name string) (observation templateRaceObservation) {
	t.Helper()
	observation.Run = name
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
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
	if _, err = cluster.workflows.Create(ctx, workflow, metav1.CreateOptions{}); err != nil {
		observation.Error = err.Error()
		return
	}
	completion, err := controller.Wait(ctx, request.Name, request.RunID, &recordingWriter{})
	if err != nil {
		observation.Error = err.Error()
		return
	}
	observation.Phase = string(completion.Phase)
	pods, err := cluster.kube.CoreV1().Pods(cluster.namespace).List(ctx, metav1.ListOptions{LabelSelector: "workflows.argoproj.io/workflow=" + name})
	if err != nil {
		observation.Error = err.Error()
		return
	}
	for _, pod := range pods.Items {
		if pod.Labels["oberth.ci/role"] == "executor-identity" {
			continue
		}
		for _, container := range []string{"main", "wait"} {
			logs, err := cluster.kube.CoreV1().Pods(cluster.namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: container}).DoRaw(ctx)
			if err != nil {
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
			if err = os.WriteFile(filepath.Join(t.ArtifactDir(), "wait.log"), logs, 0o600); err != nil {
				observation.Error = err.Error()
				return
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
