//go:build linux

// These tests exercise the Linux server and Git receive path.

package gitcache_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

type rsObservation struct {
	refs       map[string]string
	record     bool
	locks      []string
	deliveries []rsDelivery
}

// final is the next server start after the stops: startup replay, then the
// pusher's retry, then the judgement and one ordinary push to the same ref,
// then, if a receive keep ref is left, repository collection, after which no
// receive may retain anything.
// It returns the verdict, the repeated deliveries, the receive keep refs
// left before collection, and the problems.
func (r *rsRun) final(t *testing.T) (string, int, int, []string) {
	t.Helper()
	rsRemove(t, r.control, "fail-once", "target", "native")
	if r.sc.Debris {
		debris := filepath.Join(r.f.Store(), ".oberth-record-"+strconv.FormatUint(r.sc.Seed, 10))
		if err := os.WriteFile(debris, []byte(`{"prepared":{},"before":{`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	c := rsOpen(t, r.cfg)
	consumer := rsConsumer(r.control, "final", func(string) {})
	var problems []string
	if err := c.ReplayPending(t.Context(), consumer); err != nil {
		problems = append(problems, "startup replay failed: "+err.Error())
	}
	if err := c.RetryReceive(t.Context(), gittest.Repository, consumer); err != nil {
		problems = append(problems, "retry failed: "+err.Error())
	}
	obs := r.observe(t)
	verdict, duplicates, found := r.judge(obs)
	problems = append(problems, found...)
	problems = append(problems, r.followUp(t, c, obs)...)
	leaked := r.leakedKeeps(t)
	if leaked == 0 {
		return verdict, duplicates, 0, problems
	}
	if err := c.GC(t.Context()); err != nil {
		problems = append(problems, "repository collection failed: "+err.Error())
	}
	if n := r.leakedKeeps(t); n > 0 {
		problems = append(problems, fmt.Sprintf("%d receive keep refs survive repository collection", n))
	}
	return verdict, duplicates, leaked, problems
}

// leakedKeeps counts retention refs of receives. Left by a stop after the
// record removal, they hold objects only and block no push; repository
// collection must delete them, as their owner is not live.
func (r *rsRun) leakedKeeps(t *testing.T) int {
	t.Helper()
	n := 0
	for _, ref := range strings.Fields(r.f.Git(t, r.f.Store(), "for-each-ref", "--format=%(refname)", "refs/oberth/keep/")) {
		if strings.HasPrefix(ref, "refs/oberth/keep/receive-") {
			n++
		}
	}
	return n
}

// readEvents reads where a phase stopped from the shared event log.
func (r *rsRun) readEvents(t *testing.T, phase *rsPhase, stop rsStop) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(r.control, "events"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, rsTransportStream) {
			phase.TransportEvents++
		} else if line != "" {
			phase.Events++
		}
	}
	phase.Delivered = strings.Contains(string(data), " deliver-saved")
	phase.Tail = strings.Join(lines[max(0, len(lines)-6):], " | ")
	if !phase.Stopped {
		return
	}
	if marker, err := os.ReadFile(filepath.Join(r.control, "native-stop")); err == nil {
		phase.Label = "native rename " + stop.When + " " + stop.Needle
		phase.Tail += " | native " + strings.ReplaceAll(strings.TrimSpace(string(marker)), "\n", " ")
		return
	}
	ordinal := strconv.Itoa(stop.Event)
	if stop.Transport != 0 {
		ordinal = rsTransportStream + strconv.Itoa(stop.Transport)
	}
	for _, line := range lines {
		if fields := strings.Fields(line); len(fields) > 3 && fields[0] == ordinal {
			phase.Label = rsLabel(fields[1], fields[3:])
		}
	}
}

// rsLabel names a stop point without object IDs, PIDs or option values.
func rsLabel(source string, words []string) string {
	kept := []string{source}
	for _, w := range words {
		if w == "-c" || w == "." || strings.Contains(w, "=") || strings.HasPrefix(w, "%(") || len(w) >= 32 {
			continue
		}
		kept = append(kept, w)
	}
	return strings.Join(kept[:min(len(kept), 6)], " ")
}

func (r *rsRun) observe(t *testing.T) rsObservation {
	t.Helper()
	obs := rsObservation{refs: rsRefs(t, r.f), locks: reviewLocks(t, r.f), deliveries: r.deliveries(t)}
	_, err := os.Stat(filepath.Join(r.f.Store(), "oberth-receive.json"))
	obs.record = !os.IsNotExist(err)
	return obs
}

func (r *rsRun) deliveries(t *testing.T) []rsDelivery {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(r.control, "deliveries"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []rsDelivery
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var d rsDelivery
		if err = json.Unmarshal([]byte(line), &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

// judge holds the invariant per push and per reference (a partially
// prepared push settles each reference on its own): a reference is at its
// old value and never delivered, or at its pushed value and delivered under
// exactly one event ID whose updates are exactly the accepted references.
func (r *rsRun) judge(obs rsObservation) (string, int, []string) {
	var problems []string
	if obs.record {
		problems = append(problems, "receive record left after recovery and retry")
	}
	if len(obs.locks) > 0 {
		problems = append(problems, fmt.Sprintf("lock files left: %v", obs.locks))
	}
	problems = append(problems, r.checkValues(obs.refs)...)
	accepted := make([][]gitcache.RefUpdate, len(r.pushes))
	for i, push := range r.pushes {
		for _, u := range push {
			if obs.refs[u.Ref] == u.NewSHA && u.NewSHA != r.before[u.Ref] {
				accepted[i] = append(accepted[i], u)
			}
		}
		problems = append(problems, r.checkAcknowledged(i, accepted[i])...)
	}
	duplicates, found := r.checkDeliveries(obs.deliveries, accepted)
	return r.verdict(accepted), duplicates, append(problems, found...)
}

// checkValues allows each reference only its old value or a pushed value.
func (r *rsRun) checkValues(refs map[string]string) []string {
	allowed := make(map[string][]string)
	for name, oid := range r.before {
		allowed[name] = append(allowed[name], oid)
	}
	for _, push := range r.pushes {
		for _, u := range push {
			allowed[u.Ref] = append(allowed[u.Ref], u.NewSHA)
			if r.before[u.Ref] == "" {
				allowed[u.Ref] = append(allowed[u.Ref], "")
			}
		}
	}
	var problems []string
	for name := range allowed {
		if !slices.Contains(allowed[name], refs[name]) {
			problems = append(problems, fmt.Sprintf("%s at %q, neither its old nor a pushed value", name, refs[name]))
		}
	}
	for name := range refs {
		if _, ok := allowed[name]; !ok {
			problems = append(problems, "unexpected reference "+name)
		}
	}
	return problems
}

// checkAcknowledged holds a receive to what its pusher read. A refusal is
// shown only after "rejected" is durable, so a refusal the pusher read must
// end rejected. Excluded, as documented behaviour: a refusal whose record
// write fails stays in memory, and a restart may deliver the prepared part
// (markReceiveRejected, TestUnwrittenRefusalIsLostAcrossRestart). That shape
// needs a failing write, which this test never injects; a stop before the
// write has shown the pusher nothing, so either outcome is then allowed. A
// completed receive's reference reported ok and not named in a restoration
// notice must be accepted.
func (r *rsRun) checkAcknowledged(i int, accepted []gitcache.RefUpdate) []string {
	tag := strconv.Itoa(i)
	status, _ := os.ReadFile(filepath.Join(r.control, "output-"+tag))
	notices, _ := os.ReadFile(filepath.Join(r.control, "diagnostics-"+tag))
	var problems []string
	// Hook refusals reach the pusher on Git's side band, the server's own
	// words on diagnostics; every refusal reason starts "push refused".
	if (bytes.Contains(notices, []byte("push refused")) || bytes.Contains(status, []byte("push refused"))) && len(accepted) > 0 {
		problems = append(problems, fmt.Sprintf("receive %d read a refusal but kept %v", i, accepted))
	}
	if first := r.phases[0]; first.Stopped || first.Errs[i] != "" {
		return problems
	}
	for _, u := range r.pushes[i] {
		told := bytes.Contains(status, []byte("ok "+u.Ref+"\n"))
		named := bytes.Contains(notices, []byte("Reference: "+u.Ref+"."))
		if told && !named && !slices.Contains(accepted, u) {
			problems = append(problems, fmt.Sprintf("receive %d returned success with %s ok, but it was not kept", i, u.Ref))
		}
	}
	return problems
}

// checkDeliveries maps each delivered event ID to the push it carries. A
// repeated delivery of one ID is the documented at-least-once contract and
// is accepted only when a stop fell after the consumer saved the event.
func (r *rsRun) checkDeliveries(deliveries []rsDelivery, accepted [][]gitcache.RefUpdate) (int, []string) {
	byID := make(map[string][]rsDelivery)
	for _, d := range deliveries {
		if d.Phase != "follow" {
			byID[d.ID] = append(byID[d.ID], d)
		}
	}
	var problems []string
	duplicates := 0
	ids := make([][]string, len(r.pushes))
	for id, list := range byID {
		duplicates += len(list) - 1
		problems = append(problems, r.checkRepeats(id, list)...)
		owner := r.owner(list[0].Updates)
		if owner < 0 {
			problems = append(problems, fmt.Sprintf("event %s delivered updates of no push: %+v", id, list[0].Updates))
			continue
		}
		ids[owner] = append(ids[owner], id)
	}
	return duplicates, append(problems, r.checkOwners(byID, ids, accepted)...)
}

func (r *rsRun) checkRepeats(id string, list []rsDelivery) []string {
	var problems []string
	for _, d := range list[1:] {
		if !sameUpdates(d.Updates, list[0].Updates) {
			problems = append(problems, "event "+id+" delivered with different updates")
		}
	}
	if len(list) > 1 && !r.redeliveryAllowed() {
		problems = append(problems, fmt.Sprintf("event %s delivered %d times without a stop after its save", id, len(list)))
	}
	return problems
}

func (r *rsRun) checkOwners(byID map[string][]rsDelivery, ids [][]string, accepted [][]gitcache.RefUpdate) []string {
	var problems []string
	for i := range r.pushes {
		switch {
		case len(accepted[i]) == 0 && len(ids[i]) > 0:
			problems = append(problems, fmt.Sprintf("push %d kept no reference but was delivered as %v", i, ids[i]))
		case len(accepted[i]) > 0 && len(ids[i]) != 1:
			problems = append(problems, fmt.Sprintf("push %d kept %v but was delivered under %d event IDs", i, accepted[i], len(ids[i])))
		case len(accepted[i]) > 0 && !sameUpdates(byID[ids[i][0]][0].Updates, accepted[i]):
			problems = append(problems, fmt.Sprintf("push %d kept %v but its event carried %v", i, accepted[i], byID[ids[i][0]][0].Updates))
		}
	}
	return problems
}

func (r *rsRun) redeliveryAllowed() bool {
	for _, phase := range r.phases {
		if phase.Stopped && phase.Delivered {
			return true
		}
	}
	return false
}

func (r *rsRun) owner(updates []gitcache.RefUpdate) int {
	for i, push := range r.pushes {
		inside := len(updates) > 0
		for _, u := range updates {
			inside = inside && slices.Contains(push, u)
		}
		if inside {
			return i
		}
	}
	return -1
}

func sameUpdates(a, b []gitcache.RefUpdate) bool {
	key := func(u gitcache.RefUpdate) string { return u.Ref + " " + u.OldSHA + " " + u.NewSHA }
	ka, kb := make([]string, 0, len(a)), make([]string, 0, len(b))
	for _, u := range a {
		ka = append(ka, key(u))
	}
	for _, u := range b {
		kb = append(kb, key(u))
	}
	sort.Strings(ka)
	sort.Strings(kb)
	return slices.Equal(ka, kb)
}

func (r *rsRun) verdict(accepted [][]gitcache.RefUpdate) string {
	for i, kept := range accepted {
		switch {
		case len(kept) == 0:
		case r.sc.Race:
			return "accepted-" + strconv.Itoa(i)
		case len(kept) == len(r.pushes[i]):
			return "accepted"
		default:
			return "partial"
		}
	}
	return "rejected"
}

// followUp is the next ordinary push to the same reference: it must be
// accepted and delivered once, under its own event ID.
func (r *rsRun) followUp(t *testing.T, c *gitcache.Cache, obs rsObservation) []string {
	t.Helper()
	ref, old := r.followRef(obs.refs)
	update := gitcache.RefUpdate{Ref: ref, OldSHA: old, NewSHA: r.follow}
	var diagnostics bytes.Buffer
	err := c.Receive(t.Context(), gitcache.ReceiveRequest{
		Repository: gittest.Repository, Actor: "agent", Fingerprint: "key", AlwaysRun: true,
		Admission: gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main", SHA: r.f.Commit},
		Input:     receiveBatchInput(t, r.f, []gitcache.RefUpdate{update}, nil), Output: io.Discard,
		Diagnostics: &diagnostics, Deliver: rsConsumer(r.control, "follow", func(string) {}),
	})
	var problems []string
	if err != nil {
		problems = append(problems, fmt.Sprintf("next push to %s refused: %v (%s)", ref, err, diagnostics.String()))
	}
	if got := rsRefs(t, r.f)[ref]; got != r.follow {
		problems = append(problems, fmt.Sprintf("next push left %s at %q", ref, got))
	}
	var follow []rsDelivery
	for _, d := range r.deliveries(t) {
		if d.Phase == "follow" {
			follow = append(follow, d)
		}
	}
	if len(follow) != 1 || !sameUpdates(follow[0].Updates, []gitcache.RefUpdate{update}) {
		problems = append(problems, fmt.Sprintf("next push delivered %+v", follow))
	}
	if _, err = os.Stat(filepath.Join(r.f.Store(), "oberth-receive.json")); !os.IsNotExist(err) {
		problems = append(problems, "next push left a receive record")
	}
	return problems
}

// followRef prefers a pushed branch that exists, then a pushed name that can
// be created again, then topic (an accepted tag cannot be pushed twice).
func (r *rsRun) followRef(refs map[string]string) (string, string) {
	for _, push := range r.pushes {
		for _, u := range push {
			if strings.HasPrefix(u.Ref, "refs/heads/") && refs[u.Ref] != "" {
				return u.Ref, refs[u.Ref]
			}
		}
	}
	for _, push := range r.pushes {
		for _, u := range push {
			if refs[u.Ref] == "" && !rsConflicts(refs, u.Ref) {
				return u.Ref, ""
			}
		}
	}
	return "refs/heads/topic", refs["refs/heads/topic"]
}

func rsConflicts(refs map[string]string, name string) bool {
	for existing := range refs {
		if strings.HasPrefix(existing, name+"/") || strings.HasPrefix(name, existing+"/") {
			return true
		}
	}
	return false
}

type rsResult struct {
	verdict    string
	phases     []rsPhase
	duplicates int
	leaked     int
}

type rsStats struct {
	mu           sync.Mutex
	runs         int
	stopped      int
	duplicates   int
	leaked       int
	maxEvents    int
	maxTransport int
	verdicts     map[string]int
	labels       map[string]int
}

func (s *rsStats) add(result rsResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.verdicts == nil {
		s.verdicts, s.labels = make(map[string]int), make(map[string]int)
	}
	s.runs++
	s.duplicates += result.duplicates
	s.leaked += result.leaked
	s.verdicts[result.verdict]++
	for i, phase := range result.phases {
		if !phase.Stopped {
			s.maxEvents = max(s.maxEvents, phase.Events)
			s.maxTransport = max(s.maxTransport, phase.TransportEvents)
			s.labels[fmt.Sprintf("%d: no stop", i+1)]++
			continue
		}
		s.stopped++
		s.labels[fmt.Sprintf("%d: %s", i+1, phase.Label)]++
	}
}

// exercised fails a fixed seed list that no longer reaches both outcomes or
// any stop, so a broken fixture cannot pass by rejecting everything.
func (s *rsStats) exercised(t *testing.T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	accepted := 0
	for verdict, n := range s.verdicts {
		if strings.HasPrefix(verdict, "accepted") {
			accepted += n
		}
	}
	if s.runs > 0 && (accepted == 0 || s.verdicts["rejected"] == 0 || s.stopped == 0) {
		t.Errorf("seed list exercised too little: %d stops, outcomes %v", s.stopped, s.verdicts)
	}
}

func (s *rsStats) log(t *testing.T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t.Logf("runs %d, stops %d, repeated deliveries %d, receive keep refs left for collection %d, "+
		"most events in an unstopped phase %d main / %d transport, outcomes %v",
		s.runs, s.stopped, s.duplicates, s.leaked, s.maxEvents, s.maxTransport, s.verdicts)
	labels := make([]string, 0, len(s.labels))
	for label, n := range s.labels {
		labels = append(labels, fmt.Sprintf("%4d  %s", n, label))
	}
	sort.Strings(labels)
	t.Logf("stop points:\n%s", strings.Join(labels, "\n"))
}
