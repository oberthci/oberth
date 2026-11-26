package argojob

import (
	"net"
	"testing"
	"time"
)

func TestReleaseStopsExecBeforeReplyCompletes(t *testing.T) {
	o, path, c := testHoldOwner(t)
	started := make(chan *inspection, 1)
	o.runtime.start = func(argv []string) (*inspection, error) {
		p, err := startInspection(argv)
		if err == nil {
			started <- p
		}
		return p, err
	}
	_, next := sendHold(t, path, holdRequest{Version: holdProtocol, UID: "uid", Op: "exec", Argv: []string{"sh", "-c", "kill -STOP $$"}})
	child := <-started
	cleanupInspection(t, child)
	<-c.timers
	server, client := net.Pipe()
	defer client.Close()
	released := make(chan struct{})
	go func() { o.request(server); close(released) }()
	if err := writeHoldFrame(client, holdRequest{Version: holdProtocol, UID: "uid", Op: "release"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-o.stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("release did not publish stopped")
	}
	select {
	case <-o.done:
		t.Fatal("owner exited before release reply could be read")
	default:
	}
	if f := finalHold(t, next); f.Type != "closed" {
		t.Fatal("released exec reported a command result", f)
	}
	var reply holdFrame
	if err := readHoldFrame(client, &reply); err != nil || reply.Type != "closed" {
		t.Fatal(reply, err)
	}
	<-released
	select {
	case <-o.done:
	default:
		t.Fatal("owner did not finish after release reply")
	}
}

func TestNaturalInspectionSignalRetainsExit(t *testing.T) {
	_, path, _ := testHoldOwner(t)
	_, next := sendHold(t, path, holdRequest{Version: holdProtocol, UID: "uid", Op: "exec", Argv: []string{"sh", "-c", "kill -KILL $$"}})
	if f := finalHold(t, next); f.Type != "exit" || f.Code != 137 {
		t.Fatal("natural child result reclassified as a hold stop", f)
	}
}
