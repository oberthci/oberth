package app

import (
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func TestHealthReportsControllerProfileOutageWithoutAPIRead(t *testing.T) {
	profile := model.ControllerProfileStatus{State: "blocked", ErrorClass: "throttled", ReadFailuresTotal: 3}
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
