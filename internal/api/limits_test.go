package api

import (
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
)

func TestSessionLimitsBelongToIdentityAndInstallation(t *testing.T) {
	ss := sessions{items: map[string]*session{}}
	defer ss.close()
	nonce := 0
	rt := Runtime{Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }, Nonce: func() string { nonce++; return fmt.Sprint(nonce) }}
	c := apicontract.Caller{}
	for identity := 0; identity < 8; identity++ {
		c.Actor.Identity = fmt.Sprint(identity)
		for n := 0; n < 64; n++ {
			c.CredentialID = fmt.Sprint(n)
			if _, err := ss.create(c, rt); err != nil {
				t.Fatal(identity, n, err)
			}
		}
		if _, err := ss.create(c, rt); err == nil || !strings.Contains(err.Error(), "identity") {
			t.Fatal("identity session bound", err)
		}
	}
	c.Actor.Identity = "another"
	if _, err := ss.create(c, rt); err == nil || !strings.Contains(err.Error(), "installation") {
		t.Fatal("installation session bound", err)
	}
	ss.remove("1")
	if _, err := ss.create(c, rt); err != nil {
		t.Fatal("closed session did not release capacity", err)
	}
}

func TestIdleSessionDeletesItselfWithoutAnotherRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ss := sessions{items: map[string]*session{}}
		defer ss.close()
		rt := Runtime{Now: func() time.Time { timer := time.NewTimer(0); defer timer.Stop(); return <-timer.C }, Nonce: func() string { return "session" }}
		if _, err := ss.create(apicontract.Caller{}, rt); err != nil {
			t.Fatal(err)
		}
		timer := time.NewTimer(time.Hour)
		<-timer.C
		synctest.Wait()
		ss.mu.Lock()
		defer ss.mu.Unlock()
		if len(ss.items) != 0 {
			t.Fatal("idle session retained without traffic")
		}
	})
}

func TestStreamLimitsReserveRoomForOtherIdentities(t *testing.T) {
	for _, tc := range []struct {
		kind string
		per  int
	}{{"events", 32}, {"native", 64}} {
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
