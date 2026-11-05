package argojob

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestExecutionPlacementExpiredSnapshotDoesNotAdmit(t *testing.T) {
	config, request, objects := placementFixture(t)
	p := config.ExecutionPlacement
	p.document.Expires = time.Now().Add(-time.Minute)
	p.digest = placementHash(p.document)
	body, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	var retained Config
	if err := json.Unmarshal(body, &retained); err != nil {
		t.Fatal("expired custody cannot decode", err)
	}
	if _, err := DefaultedConfig(retained); err != nil {
		t.Fatal("expired custody cannot reconstruct", err)
	}
	after, err := json.Marshal(retained)
	if err != nil || !bytes.Equal(body, after) || retained.ExecutionPlacement.digest != p.digest {
		t.Fatal("decoding renewed or changed original authority", err)
	}
	policy, _ := json.Marshal(p)
	if _, err := ParseExecutionPlacement(policy); err == nil {
		t.Fatal("expired policy admitted")
	}
	if _, err := Build(retained, request); err == nil {
		t.Fatal("expired Build admitted")
	}
	client := placementClient(objects...)
	if err := retained.ExecutionPlacement.verifyEnvironment(t.Context(), client, retained); err == nil {
		t.Fatal("expired environment admitted")
	}
	seeder := NewSourceSeeder(client, recordingExec(nil), retained)
	name := sourceClaimName(request.Name)
	if _, err := seeder.createSeedPodBound(t.Context(), name, name, &request); err == nil {
		t.Fatal("expired source allocation admitted")
	}
	if len(client.Actions()) != 0 {
		t.Fatal("expired admission reached target", client.Actions())
	}
	tolerations, err := p.admittedTolerations("seed")
	if err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: p.document.Namespace, UID: "original-pod", ResourceVersion: "1"}, Spec: corev1.PodSpec{ServiceAccountName: p.document.ServiceAccount, NodeName: p.document.NodeName, NodeSelector: p.selectors(), Tolerations: tolerations}}
	if err := retained.ExecutionPlacement.verifyPodPlacement(pod, "seed"); err != nil {
		t.Fatal("original observation blocked after expiry", err)
	}
	pod.Spec.NodeName = "replacement-node"
	if err := retained.ExecutionPlacement.verifyPodPlacement(pod, "seed"); err == nil {
		t.Fatal("expired observation lost placement fence")
	}
	p.document.Expires = time.Time{}
	policy, _ = json.Marshal(p)
	if err := json.Unmarshal(policy, new(ExecutionPlacement)); err == nil {
		t.Fatal("invalid retained lifetime accepted")
	}
}
