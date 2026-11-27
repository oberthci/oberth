package api

import (
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
)

func TestStreamLimitsReserveRoomForOtherIdentities(t *testing.T) {
	for _, tc := range []struct {
		kind string
		per  int
	}{{"native", 64}} {
		t.Run(tc.kind, func(t *testing.T) {
			checkStreamLimits(t, tc.kind, tc.per)
		})
	}
}

func checkStreamLimits(t *testing.T, kind string, per int) {
	r := reservations{counts: map[string]int{}}
	var releases []func()
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	for n := 0; n < 256; n++ {
		identity := fmt.Sprint(n / per)
		release, err := r.stream(kind, identity, per)
		if err != nil {
			t.Fatal(n, err)
		}
		releases = append(releases, release)
		if (n+1)%per == 0 {
			if _, err = r.stream(kind, identity, per); err == nil || !strings.Contains(err.Error(), "identity") {
				t.Fatal("identity stream bound", err)
			}
		}
	}
	if _, err := r.stream(kind, "new", per); err == nil || !strings.Contains(err.Error(), "installation") {
		t.Fatal("installation stream bound", err)
	}
	releases[0]()
	release, err := r.stream(kind, "new", per)
	if err != nil {
		t.Fatal("capacity not released", err)
	}
	release()
}

func TestLongPollQuotaSharesIdentityAcrossCredentials(t *testing.T) {
	s := &Server{reservations: reservations{counts: map[string]int{}}}
	caller := apicontract.Caller{}
	caller.Actor.Identity = "uplink"
	var releases []func()
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	for n := 0; n < 64; n++ {
		caller.CredentialID = fmt.Sprint(n % 20)
		release, err := s.reserveCall(caller, apicontract.Metadata{LongPoll: true})
		if err != nil {
			t.Fatal(n, err)
		}
		releases = append(releases, release)
	}
	caller.CredentialID = "rotated"
	if _, err := s.reserveCall(caller, apicontract.Metadata{LongPoll: true}); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatal("credential rotation bypassed the identity quota", err)
	}
	if release, err := s.reserveCall(caller, apicontract.Metadata{}); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
	other := caller
	other.Actor.Identity = "another"
	if release, err := s.reserveCall(other, apicontract.Metadata{LongPoll: true}); err != nil {
		t.Fatal("another identity lost its slots", err)
	} else {
		release()
	}
	releases[0]()
	if release, err := s.reserveCall(caller, apicontract.Metadata{LongPoll: true}); err != nil {
		t.Fatal("finished poll retained its slot", err)
	} else {
		release()
	}
}
