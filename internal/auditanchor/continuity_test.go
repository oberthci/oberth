package auditanchor

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestKubernetesContinuitySurvivesRestartAndRejectsRollback(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	continuity, err := NewKubernetesContinuity(client, "oberth")
	if err != nil {
		t.Fatal(err)
	}
	history := continuityTestHistory()
	if err := continuity.Reconcile(ctx, history); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}

	records, err := client.CoreV1().ConfigMaps("oberth").List(ctx, metav1.ListOptions{
		LabelSelector: continuityLabelKey + "=" + continuityLabelValue,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(records.Items) != len(history) {
		t.Fatalf("continuity records = %d, want %d", len(records.Items), len(history))
	}
	pinned, err := continuity.Pinned(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pinned) != len(history) || !sameAuditWitness(pinned[0], history[0]) || !sameAuditWitness(pinned[1], history[1]) {
		t.Fatalf("pinned continuity history = %#v, want %#v", pinned, history)
	}
	for _, record := range records.Items {
		if record.Immutable == nil || !*record.Immutable {
			t.Fatalf("continuity record %s is mutable", record.Name)
		}
	}

	// A new process sees the same cluster-owned continuity records. Rekor may
	// return a self-consistent old prefix after a database rollback, but that
	// prefix cannot make the immutable suffix disappear.
	restarted, err := NewKubernetesContinuity(client, "oberth")
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Reconcile(ctx, history); err != nil {
		t.Fatalf("idempotent reconcile after restart: %v", err)
	}
	if err := restarted.Reconcile(ctx, history[:1]); err == nil || !strings.Contains(err.Error(), "omitted immutable continuity record") {
		t.Fatalf("rollback error = %v", err)
	}

	first, err := continuityConfigMap("oberth", 1, history[0])
	if err != nil {
		t.Fatal(err)
	}
	tampered, err := client.Tracker().Get(
		schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}, "oberth", first.Name,
	)
	if err != nil {
		t.Fatal(err)
	}
	tamperedRecord := tampered.(*corev1.ConfigMap).DeepCopy()
	tamperedRecord.Data["record.json"] = `{"tampered":true}`
	if err := client.Tracker().Update(
		schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}, tamperedRecord, "oberth",
	); err != nil {
		t.Fatal(err)
	}
	tamperCheck, err := NewKubernetesContinuity(client, "oberth")
	if err != nil {
		t.Fatal(err)
	}
	if err := tamperCheck.Reconcile(ctx, history); err == nil || !strings.Contains(err.Error(), "differs from verified Rekor history") {
		t.Fatalf("tampered continuity error = %v", err)
	}

	for _, action := range client.Actions() {
		switch action.GetVerb() {
		case "update", "patch", "delete", "deletecollection":
			t.Fatalf("continuity attempted destructive Kubernetes verb %q", action.GetVerb())
		}
	}
}

func TestKubernetesContinuityReconcilesAmbiguousCreateByExactReadback(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "configmaps", func(action ktesting.Action) (bool, runtime.Object, error) {
		create := action.(ktesting.CreateAction)
		record := create.GetObject().(*corev1.ConfigMap).DeepCopy()
		err := client.Tracker().Create(
			schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}, record, "oberth",
		)
		if err != nil {
			return true, nil, err
		}
		return true, nil, context.DeadlineExceeded
	})
	continuity, err := NewKubernetesContinuity(client, "oberth")
	if err != nil {
		t.Fatal(err)
	}
	if err := continuity.Reconcile(ctx, continuityTestHistory()[:1]); err != nil {
		t.Fatalf("ambiguous create with exact readback: %v", err)
	}
	var sawGet bool
	for _, action := range client.Actions() {
		if action.GetVerb() == "get" {
			sawGet = true
		}
	}
	if !sawGet {
		t.Fatal("ambiguous create did not perform exact readback")
	}
}

func TestContinuityCreateReadbackOutlivesCanceledRequestWithinBound(t *testing.T) {
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	cancelRequest()

	readbackCtx, cancelReadback := continuityCreateReadbackContext(requestCtx)
	defer cancelReadback()
	if err := readbackCtx.Err(); err != nil {
		t.Fatalf("detached readback inherited canceled request: %v", err)
	}
	deadline, ok := readbackCtx.Deadline()
	if !ok {
		t.Fatal("detached readback has no finite deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > continuityCreateReadbackLimit {
		t.Fatalf("detached readback deadline remaining = %s, want (0, %s]", remaining, continuityCreateReadbackLimit)
	}
}

func TestKubernetesContinuityRejectsHiddenNameConflict(t *testing.T) {
	ctx := context.Background()
	history := continuityTestHistory()[:1]
	wanted, err := continuityConfigMap("oberth", 1, history[0])
	if err != nil {
		t.Fatal(err)
	}
	conflict := wanted.DeepCopy()
	conflict.Labels = nil
	conflict.Data = map[string]string{"record.json": `{"other":true}`}
	client := fake.NewSimpleClientset(conflict)
	continuity, err := NewKubernetesContinuity(client, "oberth")
	if err != nil {
		t.Fatal(err)
	}
	if err := continuity.Reconcile(ctx, history); err == nil || !strings.Contains(err.Error(), "different immutable content") {
		t.Fatalf("hidden name conflict error = %v", err)
	}
}

func TestKubernetesContinuityRequiresValidLinkedHistory(t *testing.T) {
	client := fake.NewSimpleClientset()
	continuity, err := NewKubernetesContinuity(client, "oberth")
	if err != nil {
		t.Fatal(err)
	}
	history := continuityTestHistory()
	history[1].PreviousUUID = "wrong"
	if err := continuity.Reconcile(context.Background(), history); err == nil || !strings.Contains(err.Error(), "does not link") {
		t.Fatalf("unlinked history error = %v", err)
	}
	if len(client.Actions()) != 0 {
		t.Fatalf("invalid history reached Kubernetes: %#v", client.Actions())
	}
	if _, err := NewKubernetesContinuity(nil, "oberth"); err == nil {
		t.Fatal("nil Kubernetes client accepted")
	}
	if _, err := NewKubernetesContinuity(client, ""); err == nil {
		t.Fatal("empty namespace accepted")
	}
}

func TestContinuityValidationHasNoFixedOperationalLifetime(t *testing.T) {
	const witnessCount = 4097
	history := continuityLongHistory(witnessCount)
	if err := validateContinuityHistory(history); err != nil {
		t.Fatalf("legitimate long-lived witness history rejected: %v", err)
	}
	if _, err := continuityConfigMap("oberth", witnessCount, history[len(history)-1]); err != nil {
		t.Fatalf("long-lived continuity record: %v", err)
	}
}

func TestKubernetesContinuityReloadsCompleteExternalHistories(t *testing.T) {
	ctx := context.Background()
	history := continuityTestHistory()

	t.Run("missing older completion", func(t *testing.T) {
		records := make([]runtime.Object, 0, len(history))
		for index, witness := range history {
			record, err := continuityConfigMap("oberth", index+1, witness)
			if err != nil {
				t.Fatal(err)
			}
			records = append(records, record)
		}
		client := fake.NewSimpleClientset(records...)
		continuity, err := NewKubernetesContinuity(client, "oberth")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := continuity.Pinned(ctx); err != nil {
			t.Fatal(err)
		}
		first := records[0].(*corev1.ConfigMap)
		if err := client.Tracker().Delete(
			schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}, "oberth", first.Name,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := continuity.Pinned(ctx); err == nil {
			t.Fatal("cached continuity accepted history with an externally removed older record")
		}
	})

	t.Run("appended intent", func(t *testing.T) {
		firstIntent := model.AuditWitnessIntent{
			Sequence: 1, AuditID: history[0].AuditID, AuditSHA256: history[0].AuditSHA256,
		}
		first, err := continuityIntentConfigMap("oberth", firstIntent)
		if err != nil {
			t.Fatal(err)
		}
		client := fake.NewSimpleClientset(first)
		continuity, err := NewKubernetesContinuity(client, "oberth")
		if err != nil {
			t.Fatal(err)
		}
		if intents, err := continuity.Intents(ctx); err != nil || len(intents) != 1 {
			t.Fatalf("initial intents = %#v, %v", intents, err)
		}
		secondIntent := model.AuditWitnessIntent{
			Sequence: 2, AuditID: history[1].AuditID, AuditSHA256: history[1].AuditSHA256,
			PreviousUUID: history[0].UUID,
		}
		second, err := continuityIntentConfigMap("oberth", secondIntent)
		if err != nil {
			t.Fatal(err)
		}
		if err := client.Tracker().Create(
			schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}, second, "oberth",
		); err != nil {
			t.Fatal(err)
		}
		intents, err := continuity.Intents(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(intents) != 2 || !sameAuditWitnessIntent(intents[1], secondIntent) {
			t.Fatalf("reloaded intents = %#v, want externally appended intent %#v", intents, secondIntent)
		}
	})
}

func TestKubernetesContinuityPagesCompleteLongHistoryOnEveryRead(t *testing.T) {
	const witnessCount = 1001
	ctx := context.Background()
	history := continuityLongHistory(witnessCount)
	records := make([]corev1.ConfigMap, 0, len(history))
	objects := make([]runtime.Object, 0, len(history))
	for index, witness := range history {
		record, err := continuityConfigMap("oberth", index+1, witness)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, *record.DeepCopy())
		objects = append(objects, record)
	}
	client := fake.NewSimpleClientset(objects...)
	listCalls := 0
	client.PrependReactor("list", "configmaps", func(action ktesting.Action) (bool, runtime.Object, error) {
		listAction, ok := action.(ktesting.ListActionImpl)
		if !ok {
			return true, nil, fmt.Errorf("unexpected list action type %T", action)
		}
		options := listAction.GetListOptions()
		if options.LabelSelector != continuityLabelKey+"="+continuityLabelValue {
			return false, nil, nil
		}
		if options.Limit != continuityListPageSize {
			return true, nil, fmt.Errorf("continuity list limit = %d, want %d", options.Limit, continuityListPageSize)
		}
		start := 0
		if options.Continue != "" {
			var err error
			start, err = strconv.Atoi(options.Continue)
			if err != nil {
				return true, nil, fmt.Errorf("parse continuation token %q: %w", options.Continue, err)
			}
		}
		end := min(start+int(options.Limit), len(records))
		if start < 0 || start > end {
			return true, nil, fmt.Errorf("invalid continuation offset %d", start)
		}
		page := &corev1.ConfigMapList{Items: append([]corev1.ConfigMap(nil), records[start:end]...)}
		if end < len(records) {
			page.Continue = strconv.Itoa(end)
		}
		listCalls++
		return true, page, nil
	})
	continuity, err := NewKubernetesContinuity(client, "oberth")
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := continuity.Pinned(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pinned) != witnessCount || listCalls != 3 {
		t.Fatalf("paged continuity load = %d records in %d calls, want %d records in 3 calls", len(pinned), listCalls, witnessCount)
	}
	if _, err := continuity.Pinned(ctx); err != nil {
		t.Fatal(err)
	}
	if listCalls != 6 {
		t.Fatalf("second paged continuity load used %d total calls, want 6", listCalls)
	}
}

func continuityLongHistory(witnessCount int) []model.AuditWitness {
	history := make([]model.AuditWitness, 0, witnessCount)
	previous := ""
	for index := 1; index <= witnessCount; index++ {
		digest := sha256.Sum256([]byte(strconv.Itoa(index)))
		uuid := fmt.Sprintf("witness-%d", index)
		history = append(history, model.AuditWitness{
			UUID: uuid, PreviousUUID: previous, LogIndex: int64(index - 1),
			IntegratedAt: time.Unix(int64(index), 0).UTC(), AuditID: int64(index), AuditSHA256: digest[:],
		})
		previous = uuid
	}
	return history
}

func continuityTestHistory() []model.AuditWitness {
	firstDigest := sha256.Sum256([]byte("first"))
	secondDigest := sha256.Sum256([]byte("second"))
	firstTime := time.Date(2026, 8, 6, 11, 0, 0, 0, time.UTC)
	return []model.AuditWitness{
		{UUID: "witness-one", LogIndex: 41, IntegratedAt: firstTime, AuditID: 1, AuditSHA256: firstDigest[:]},
		{UUID: "witness-two", PreviousUUID: "witness-one", LogIndex: 99, IntegratedAt: firstTime.Add(time.Second), AuditID: 2, AuditSHA256: secondDigest[:]},
	}
}
