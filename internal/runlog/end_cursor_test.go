package runlog

import (
	"io"
	"strings"
	"testing"
)

func TestReadAtCurrentEndRetainsItsByteCursor(t *testing.T) {
	s := newStore(t, 4096)
	capture(t, s, native("one"), "own output\n")
	request := ReadRequest{Selector: Selector{Step: "build", StepSet: true}}
	body, first, err := s.Read(t.Context(), "run", request)
	if err != nil || string(body) != "own output\n" {
		t.Fatal(string(body), first, err)
	}
	request.Filter.ByteOffset = &first.NextByteOffset
	for range 2 {
		body, next, err := s.Read(t.Context(), "run", request)
		if err != nil || len(body) != 0 || next.NextByteOffset != first.NextByteOffset || next.HasMore {
			t.Fatal("an empty poll rewound and could replay step output", string(body), next, err)
		}
	}
}

func TestOwnOutputIncludesAuthoredContainerSetOnly(t *testing.T) {
	s := newStore(t, 4096)
	for _, item := range []struct {
		name string
		own  bool
	}{
		{"init", false}, {"compile", true}, {"test", true}, {"wait", false},
	} {
		entry := native("one")
		entry.Container, entry.Main = item.name, item.own
		capture(t, s, entry, item.name+"\n")
	}
	body, _, err := s.Read(t.Context(), "run", ReadRequest{OwnOnly: true})
	if err != nil || string(body) != "compile\ntest\n" {
		t.Fatal(string(body), err)
	}
	body, _, err = s.Read(t.Context(), "run", ReadRequest{Selector: Selector{NodeID: "one", Container: "wait"}})
	if err != nil || string(body) != "wait\n" {
		t.Fatal(string(body), err)
	}
}

func TestActiveCopyRemainsVisibleBesideAnOlderAttempt(t *testing.T) {
	s := newStore(t, 4096)
	capture(t, s, native("old"), "earlier attempt\n")
	reservation, err := s.Reserve(t.Context(), "run", native("new"))
	if err != nil {
		t.Fatal(err)
	}
	request := ReadRequest{Selector: Selector{Step: "build", StepSet: true}}
	body, pending, err := s.Read(t.Context(), "run", request)
	if err != nil || !pending.Copying || string(body) != "earlier attempt\n" {
		t.Fatal(string(body), pending, err)
	}
	if err = reservation.Capture(t.Context(), CaptureRequest{Source: io.NopCloser(strings.NewReader("finished attempt\n"))}); err != nil {
		t.Fatal(err)
	}
	request.Filter.ByteOffset = &pending.NextByteOffset
	body, finished, err := s.Read(t.Context(), "run", request)
	if err != nil || finished.Copying || string(body) != "finished attempt\n" {
		t.Fatal(string(body), finished, err)
	}
}
