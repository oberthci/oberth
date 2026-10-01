package service

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

// The #552 waiting reason names the concurrency group and the exact run a
// grouped run waits on (#658), and is unchanged for a run that waits on weight
// alone, including the head of a free group.
func TestWaitingAdmissionMessageNamesTheGroupAndTheRun(t *testing.T) {
	const holder, ahead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const weightOnly = "Waiting in the weighted resource admission queue."
	for _, tc := range []struct {
		admission model.AdmissionObservation
		want      []string
	}{
		{model.AdmissionObservation{State: "waiting"}, []string{weightOnly}},
		{model.AdmissionObservation{State: "waiting", Group: "terraform-state"}, []string{weightOnly}},
		{model.AdmissionObservation{State: "waiting", Group: "terraform-state", GroupHolder: holder},
			[]string{`concurrency group "terraform-state"`, "held by run " + holder, "outside the group are admitted"}},
		{model.AdmissionObservation{State: "waiting", Group: "terraform-state", GroupAhead: ahead},
			[]string{`concurrency group "terraform-state"`, "behind run " + ahead}},
	} {
		message := waitingAdmissionMessage(&tc.admission)
		for _, want := range tc.want {
			if !strings.Contains(message, want) {
				t.Fatalf("message for %#v = %q; want it to contain %q", tc.admission, message, want)
			}
		}
	}
}
