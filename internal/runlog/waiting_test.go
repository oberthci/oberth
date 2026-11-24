package runlog

import (
	"testing"

	"github.com/oberthci/oberth/internal/logcontract"
)

func TestWaitingMetadataDistinguishesAbsentAndRetiredSources(t *testing.T) {
	t.Run("planned", checkPlannedWaitingMetadata)
	t.Run("following", checkFollowingWaitingMetadata)
	t.Run("captured", checkCapturedWaitingMetadata)
}

func waitingReadRequest() ReadRequest {
	return ReadRequest{Selector: logcontract.Selector{Step: "build", StepSet: true}, PlannedSteps: []string{"build"}, OwnOnly: true}
}

func checkPlannedWaitingMetadata(t *testing.T) {
	s := newStore(t, 1024)
	request := waitingReadRequest()
	_, meta, err := s.Read(t.Context(), "run", request)
	if err == nil || !meta.Waiting || meta.Follow || meta.Copying {
		t.Fatal("planned output did not advertise waiting", meta, err)
	}
	request.PlannedSteps = []string{"other"}
	_, meta, _ = s.Read(t.Context(), "run", request)
	if meta.Waiting {
		t.Fatal("unknown step advertises waiting", meta)
	}
	request.PlannedSteps = []string{"build"}
	entry := native("live")
	if err := s.Track(t.Context(), "run", []ContainerLog{entry}); err != nil {
		t.Fatal(err)
	}
	_, meta, _ = s.Read(t.Context(), "run", request)
	if !meta.Waiting {
		t.Fatal("waiting receipt lost its retry state", meta)
	}
}

func checkFollowingWaitingMetadata(t *testing.T) {
	s := newStore(t, 1024)
	request := waitingReadRequest()
	entry := native("live")
	if err := s.Track(t.Context(), "run", []ContainerLog{entry}); err != nil {
		t.Fatal(err)
	}
	entry.State = Following
	if err := s.Track(t.Context(), "run", []ContainerLog{entry}); err != nil {
		t.Fatal(err)
	}
	_, meta, _ := s.Read(t.Context(), "run", request)
	if meta.Waiting || !meta.Follow {
		t.Fatal("current source is still waiting", meta)
	}
	s.KeepFollowing("run", nil)
	_, meta, _ = s.Read(t.Context(), "run", request)
	if meta.Waiting || meta.Follow {
		t.Fatal("retired source advertises retry", meta)
	}
}

func checkCapturedWaitingMetadata(t *testing.T) {
	s := newStore(t, 1024)
	request := waitingReadRequest()
	capture(t, s, native("empty"), "")
	request.Selector.NodeID, request.Selector.Container = "empty", "main"
	_, meta, err := s.Read(t.Context(), "run", request)
	if err != nil || meta.Waiting || meta.Follow || meta.Copying {
		t.Fatal("empty captured output advertises retry", meta, err)
	}
}
