package auditanchor

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/sigstore/rekor/pkg/generated/models"
)

type cancelEnumerationVerifier struct{ cancel context.CancelFunc }

func (v cancelEnumerationVerifier) Verify(context.Context, *models.LogEntryAnon) error {
	v.cancel()
	return nil
}

func TestEnumerationTimeoutCountsForeignCandidates(t *testing.T) {
	api := newMemoryRekorAPI()
	publisher, err := newRekorWitness(api, &fakeRekorEntryVerifier{}, []byte("foreign host"))
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("head"))
	head := model.AuditHead{ID: 1, SHA256: hash[:]}
	if _, err = publisher.History(t.Context(), nil, head); err != nil {
		t.Fatal(err)
	}
	entry, err := publisher.Publish(t.Context(), head)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	witness, err := newRekorWitness(api, cancelEnumerationVerifier{cancel}, []byte("own host"))
	if err != nil {
		t.Fatal(err)
	}
	api.byEmail[witness.identity] = []string{entry.UUID, strings.Repeat("a", 64)}
	_, _, err = witness.AbandonedChainTip(ctx)
	if err == nil || !strings.Contains(err.Error(), "verifying 1 of 2 candidates (0 own-chain)") {
		t.Fatalf("timeout count = %v", err)
	}
}
