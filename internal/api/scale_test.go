package api

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
)

// This is admission capacity, not a latency or installation load qualification.
func TestActorScaleBudgets(t *testing.T) {
	for _, size := range []int{30, 100} {
		t.Run(fmt.Sprint(size), func(t *testing.T) { checkActorScaleBudgets(t, size) })
	}
}
func checkActorScaleBudgets(t *testing.T, size int) {
	t.Helper()
	ss := sessions{items: map[string]*session{}}
	defer ss.close()
	s := &Server{reservations: reservations{counts: map[string]int{}}}
	rt := Runtime{Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }}
	var ids []string
	var releases []func()
	for i := range size {
		c := apicontract.Caller{CredentialID: fmt.Sprintf("credential-%d", i)}
		c.Actor.Identity = fmt.Sprintf("actor-%d", i)
		rt.Nonce = func() string { return c.CredentialID }
		id, err := ss.create(c, rt)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		releases = append(releases, reserveScaleActor(t, s, c)...)
	}
	if s.reservations.counts["events"] != size || s.reservations.counts["native"] != size {
		t.Fatal("stream kind budgets merged")
	}
	drainScaleActors(t, releases, &s.reservations)
	for _, id := range ids {
		ss.remove(id)
		ss.remove(id)
	}
	if len(ss.items) != 0 {
		t.Fatal("sessions retained after drain")
	}
}
func reserveScaleActor(t *testing.T, s *Server, c apicontract.Caller) []func() {
	t.Helper()
	var releases []func()
	for range 2 {
		release, err := s.reserveCall(c, apicontract.Metadata{LongPoll: true})
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	for _, kind := range []string{"events", "native"} {
		per := eventsPerIdentity
		if kind == "native" {
			per = nativePerIdentity
		}
		release, err := s.reservations.stream(kind, c.Actor.Identity, per)
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	return releases
}
func drainScaleActors(t *testing.T, releases []func(), r *reservations) {
	t.Helper()
	var joined sync.WaitGroup
	for _, release := range releases {
		for range 2 {
			joined.Go(release)
		}
	}
	joined.Wait()
	if len(r.counts) != 0 {
		t.Fatal("release did not drain exactly once", r.counts)
	}
}

func TestActorScaleAdmissionRace(t *testing.T) {
	for _, kind := range []string{"events", "native"} {
		t.Run(kind, func(t *testing.T) {
			r := reservations{counts: map[string]int{}}
			var admitted atomic.Int64
			var joined sync.WaitGroup
			releases := make(chan func(), 512)
			for i := range 512 {
				joined.Go(func() {
					release, err := r.stream(kind, fmt.Sprint(i), 32)
					if err == nil {
						admitted.Add(1)
						releases <- release
					}
				})
			}
			joined.Wait()
			close(releases)
			if admitted.Load() != 256 {
				t.Fatal("installation admission race", admitted.Load())
			}
			for release := range releases {
				release()
				release()
			}
			if len(r.counts) != 0 {
				t.Fatal("refused or released admission leaked", r.counts)
			}
		})
	}
}
