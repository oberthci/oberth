package store

import (
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestLiveIssueLockRefusesForeignEditAndAcquire(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	issue := manualIssue(t, s)
	requireOK(t, s.ChangeIssueLock(t.Context(), stateport.Action{Actor: "owner"}, stateport.IssueLockRequest{ID: issue.ID, Owner: "owner"}))
	foreign := stateport.Action{Actor: "other"}
	_, err := s.UpdateIssue(t.Context(), foreign, model.IssuePatch{ID: issue.ID, ExpectedRevision: &issue.Revision, Title: new("foreign")})
	requireCode(t, err, publicerr.Conflict)
	requireCode(t, s.ChangeIssueLock(t.Context(), foreign, stateport.IssueLockRequest{ID: issue.ID, Owner: foreign.Actor}), publicerr.Conflict)
	got, err := s.Issue(t.Context(), issue.ID)
	requireOK(t, err)
	if got.Title != issue.Title || got.Lock == nil || got.Lock.Owner != "owner" {
		t.Fatal("foreign lock mutation")
	}
}

func TestCIReopenAndChangedDeletionReplayRefuse(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	r, input := accepted(t, s, "run")
	a := stateport.Action{Actor: "operator"}
	issue, err := s.CreateIssue(t.Context(), a, model.Issue{Kind: model.IssueCI, Title: "CI", Repo: r.Repo})
	requireOK(t, err)
	requireCode(t, s.ReopenIssue(t.Context(), a, stateport.IssueClose{ID: issue.ID, Revision: issue.Revision}), publicerr.InvalidTransition)
	request := input.Request
	request.ReceiveID = "delete"
	requireOK(t, recordDeletion(t, s, deletionForTest(request)))
	request.Assist = !request.Assist
	requireCode(t, recordDeletion(t, s, deletionForTest(request)), publicerr.Conflict)
}

func TestKeyReplayBindingsRefuse(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	a := stateport.Action{Actor: "operator"}
	key := atomicKey(t, s)
	head := independentHead(t, s.readers)
	requireOK(t, s.RegisterKey(t.Context(), a, key))
	if independentHead(t, s.readers) != head {
		t.Fatal("key replay appended audit")
	}
	key.Identity = "other"
	requireCode(t, s.RegisterKey(t.Context(), a, key), publicerr.Conflict)
	next := atomicCredential(t, s)
	requireOK(t, s.AddCredential(t.Context(), a, next))
	next.Fingerprint = "other"
	requireCode(t, s.RotateCredential(t.Context(), a, CredentialRotation{PreviousDigest: next.Digest, Next: next}), publicerr.Forbidden)
}
