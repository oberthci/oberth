package gitcache

import (
	"os"
	"strings"
	"testing"
)

func TestRepeatedPreparationDoesNotRewriteRecord(t *testing.T) {
	c, session, _ := gateFixture(t)
	record, err := readReservation(session.store)
	if err != nil {
		t.Fatal(err)
	}
	record.State = "ready"
	record.Event.Updates = []RefUpdate{{Ref: "refs/heads/one", NewSHA: strings.Repeat("a", 40)}, {Ref: "refs/heads/two", NewSHA: strings.Repeat("b", 40)}}
	if err = writeReservation(session.store, record); err != nil {
		t.Fatal(err)
	}
	if err = c.markReceivePrepared(session, record.Event.Updates[:1]); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(session.store, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(session.store, 0700) })
	if err = c.markReceivePrepared(session, record.Event.Updates[:1]); err != nil {
		t.Fatal("repeated preparation rewrote its stored fact", err)
	}
	if err = c.markReceivePrepared(session, record.Event.Updates[1:]); err == nil {
		t.Fatal("new transaction accepted without storing its preparation")
	}
}

func TestFailedRejectionWriteKeepsRetryableState(t *testing.T) {
	_, session, _ := gateFixture(t)
	record, err := readReservation(session.store)
	if err != nil {
		t.Fatal(err)
	}
	record.State = "ready"
	if err = os.Chmod(session.store, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(session.store, 0700) })
	if err = markReceiveRejected(session.store, &record); err == nil || record.State != "ready" {
		t.Fatal("failed persistence changed the recorded state", record.State, err)
	}
	if err = os.Chmod(session.store, 0700); err != nil {
		t.Fatal(err)
	}
	if err = markReceiveRejected(session.store, &record); err != nil {
		t.Fatal(err)
	}
	saved, err := readReservation(session.store)
	if err != nil || saved.State != "rejected" {
		t.Fatal("rejection retry did not persist", saved, err)
	}
}
