package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

type auditMutationVerifierMetric struct {
	FullTraversals      int     `json:"verifier_full_audit_actions_traversals"`
	ReadStatements      int     `json:"verifier_sqlite_read_statements"`
	VerifierMillis1000  float64 `json:"verifier_ms_1000"`
	VerifierMillis10000 float64 `json:"verifier_ms_10000"`
	MeasuredRows        int     `json:"measured_audit_rows"`
	Correctness         int     `json:"correctness_passed"`
	FailClosed          int     `json:"fail_closed_cases_passed"`
}

type auditMutationVerifierMeasurement struct {
	elapsed     time.Duration
	metrics     auditMutationMetrics
	rows        int
	correctness bool
}

func TestAuditMutationVerifierMetrics(t *testing.T) {
	if os.Getenv("OBERTH_AUDIT_METRICS") != "1" {
		t.Skip("set OBERTH_AUDIT_METRICS=1 to run the audit mutation verifier measurement")
	}

	measurements := make(map[int]auditMutationVerifierMeasurement, 2)
	for _, rowCount := range []int{1000, 10000} {
		measurements[rowCount] = measureAuditMutationVerifier(t, rowCount)
	}
	thousand, tenThousand := measurements[1000], measurements[10000]
	if thousand.metrics != tenThousand.metrics {
		t.Fatalf("audit mutation verifier statement metrics depend on row count: 1000=%#v 10000=%#v", thousand.metrics, tenThousand.metrics)
	}
	failClosed := measureAuditMutationVerifierFailClosed(t)
	metric := auditMutationVerifierMetric{
		FullTraversals:      tenThousand.metrics.fullTraversals,
		ReadStatements:      tenThousand.metrics.readStatements,
		VerifierMillis1000:  thousand.elapsed.Seconds() * 1000,
		VerifierMillis10000: tenThousand.elapsed.Seconds() * 1000,
		MeasuredRows:        tenThousand.rows,
		Correctness:         metricBit(thousand.correctness && tenThousand.correctness),
		FailClosed:          metricBit(failClosed),
	}
	encoded, err := json.Marshal(metric)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("AUDIT_METRICS_JSON=%s\n", encoded)
}

func measureAuditMutationVerifier(t *testing.T, rowCount int) auditMutationVerifierMeasurement {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 6, 20, 0, 0, 0, time.UTC)
	database := testStore(t, &now)
	head := seedAuditMetricRows(t, database, rowCount, now)
	anchor, err := database.RecordAuditAnchor(ctx, model.AuditAnchorSpec{
		AuditID: head.ID, AuditSHA256: head.SHA256, TSAURL: "https://tsa.example.test",
		Receipt: []byte("metric receipt"), AnchoredAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	intents := []model.AuditWitnessIntent{{Sequence: 1, AuditID: head.ID, AuditSHA256: append([]byte(nil), head.SHA256...)}}
	witnesses := []model.AuditWitness{{
		UUID: "metric-witness", LogIndex: 0, IntegratedAt: now,
		AuditID: head.ID, AuditSHA256: append([]byte(nil), head.SHA256...),
	}}

	started := time.Now()
	tx, err := database.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	verifiedHead, verifiedAnchor, metrics, err := verifyAuditMutationState(ctx, tx, intents, witnesses)
	if err == nil {
		err = tx.Commit()
	}
	elapsed := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	correctness := verifiedHead.ID == head.ID && bytes.Equal(verifiedHead.SHA256, head.SHA256) && sameMetricAnchor(verifiedAnchor, anchor)
	if !correctness {
		t.Fatalf("verified state = (%#v, %#v), want (%#v, %#v)", verifiedHead, verifiedAnchor, head, anchor)
	}
	return auditMutationVerifierMeasurement{elapsed: elapsed, metrics: metrics, rows: rowCount, correctness: correctness}
}

func measureAuditMutationVerifierFailClosed(t *testing.T) bool {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 6, 20, 0, 0, 0, time.UTC)
	database := testStore(t, &now)
	head := seedAuditMetricRows(t, database, 2, now)
	if _, err := database.RecordAuditAnchor(ctx, model.AuditAnchorSpec{
		AuditID: head.ID, AuditSHA256: head.SHA256, TSAURL: "https://tsa.example.test",
		Receipt: []byte("metric receipt"), AnchoredAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	intent := model.AuditWitnessIntent{Sequence: 1, AuditID: head.ID, AuditSHA256: append([]byte(nil), head.SHA256...)}
	witness := model.AuditWitness{
		UUID: "metric-witness", LogIndex: 0, IntegratedAt: now,
		AuditID: head.ID, AuditSHA256: append([]byte(nil), head.SHA256...),
	}
	witness.AuditSHA256[0] ^= 0xff
	tx, err := database.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	_, _, _, err = verifyAuditMutationState(ctx, tx, []model.AuditWitnessIntent{intent}, []model.AuditWitness{witness})
	if err == nil {
		t.Fatal("audit mutation gate accepted a witness whose hash is absent from the local chain")
	}
	return true
}

func metricBit(value bool) int {
	if value {
		return 1
	}
	return 0
}

func seedAuditMetricRows(t *testing.T, database *Store, count int, createdAt time.Time) model.AuditHead {
	t.Helper()
	ctx := context.Background()
	tx, err := database.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	previous := append([]byte(nil), auditGenesisSHA256...)
	for index := 1; index <= count; index++ {
		spec := model.AuditActionSpec{
			Actor: "metric@tuxbox", Action: "metric.measure", ResourceType: "metric",
			ResourceID: strconv.Itoa(index), Details: `{"fixture":true}`,
		}
		timestamp := createdAt.Add(time.Duration(index) * time.Nanosecond).UnixNano()
		digest := auditActionSHA256(int64(index), previous, spec, timestamp)
		if _, err := tx.ExecContext(ctx, `
INSERT INTO audit_actions(id, actor, action, resource_type, resource_id, details, previous_sha256, sha256, created_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`, index, spec.Actor, spec.Action, spec.ResourceType, spec.ResourceID,
			spec.Details, previous, digest, timestamp); err != nil {
			t.Fatal(err)
		}
		previous = digest
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return model.AuditHead{ID: int64(count), SHA256: append([]byte(nil), previous...)}
}

func sameMetricAnchor(left, right model.AuditAnchor) bool {
	return left.ID == right.ID && left.AuditID == right.AuditID && bytes.Equal(left.AuditSHA256, right.AuditSHA256) &&
		left.TSAURL == right.TSAURL && bytes.Equal(left.Receipt, right.Receipt) && left.AnchoredAt.Equal(right.AnchoredAt) &&
		left.CreatedAt.Equal(right.CreatedAt)
}
