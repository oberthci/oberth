package app

import (
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func TestHealthReportsControllerProfileOutageWithoutAPIRead(t *testing.T) {
	profile := model.ControllerProfileStatus{State: "blocked", ErrorClass: "invalid_profile", Reason: "argojob: controller configuration was created after the running process", ReadFailuresTotal: 3}
	health := Health{ControllerProfile: func() *model.ControllerProfileStatus { copy := profile; return &copy }}
	value, err := health.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := value.(HealthStatus).ControllerProfile
	if got == nil || *got != profile {
		t.Fatalf("missing profile verification status: %#v", got)
	}
}

func TestControllerEvictedStatusExposesPauseReasonAndRemedy(t *testing.T) {
	profile := model.ControllerProfileStatus{State: "blocked", AdmissionPaused: true, Reason: "controller ReplicaSet has not converged", Remedy: "Restore one Ready replacement"}
	health := Health{ControllerProfile: func() *model.ControllerProfileStatus { copy := profile; return &copy }}
	value, err := health.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := value.(HealthStatus).ControllerProfile
	if got == nil || !got.AdmissionPaused || got.Reason != profile.Reason || got.Remedy != profile.Remedy {
		t.Fatal("pause omitted from system status", got)
	}
}
