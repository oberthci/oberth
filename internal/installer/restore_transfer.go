package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"github.com/oberthci/oberth/internal/stateport"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

func (e *Engine) attachRestoreDatabase(ctx context.Context, namespace string, _ stateport.Options, in io.Reader) (stateport.BackupReport, error) {
	var report stateport.BackupReport
	if err := e.waitRestorePod(ctx, namespace); err != nil {
		return report, err
	}
	request := e.Cluster.Client.CoreV1().RESTClient().Post().Namespace(namespace).Resource("pods").Name("oberth-restore").SubResource("attach").VersionedParams(&corev1.PodAttachOptions{Container: "restore", Stdin: true, Stdout: true, Stderr: true}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(e.Cluster.Config, "POST", request.URL())
	if err != nil {
		return report, restoreApplyProblem()
	}
	var receipt bytes.Buffer
	out := &archiveWriter{ctx: ctx, out: &receipt, left: 64 << 10}
	if err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: in, Stdout: out, Stderr: io.Discard}); err != nil {
		return report, restoreApplyProblem()
	}
	if json.Unmarshal(receipt.Bytes(), &report) != nil {
		return report, restoreApplyProblem()
	}
	return report, nil
}

func (e *Engine) waitRestorePod(ctx context.Context, namespace string) error {
	for {
		pod, err := e.Cluster.Client.CoreV1().Pods(namespace).Get(ctx, "oberth-restore", metav1.GetOptions{})
		if err == nil && pod.Status.Phase == corev1.PodRunning && pod.Annotations["meta.helm.sh/release-name"] == "oberth" && pod.Annotations["meta.helm.sh/release-namespace"] == namespace && len(pod.Spec.Containers) == 1 && pod.Spec.Containers[0].Name == "restore" {
			return nil
		}
		if err != nil || pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			return restoreApplyProblem()
		}
		if err = e.pause(ctx); err != nil {
			return restoreApplyProblem()
		}
	}
}
