//go:build linux

// These tests exercise the Linux server and Git receive path.

package gitcache_test

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

// Random stops of the receive path. A seed fixes one push (or two racing
// pushes to one reference), its faults, and up to two stops: one in the
// receive, one in the startup recovery after it. A stop ends the whole
// server process tree at a counted point: a server callback, before or after
// each Git command and hook, a pusher read or write, or inside a Git rename
// (the package's native fault). The final instance then recovers, the
// pusher retries, an ordinary push follows, and the outcome is judged only
// from refs, the stored record, lock files and the consumer's saved events.
//
// A seed names the same stop point on every run of the same code, except
// that hooks orphaned by a "survivors" stop finish on their own schedule.
//
// OBERTH_RECEIVE_SOAK=<duration> runs random seeds for that long;
// OBERTH_RECEIVE_SEED=<n> or race:<n> replays one seed.

// The chosen stop ordinal is at most the longest observed main sequence of a
// receive (about 57 events for three references, 78 for two racing pushes),
// of startup recovery (about 31), or of a transport sequence (about 13); past
// a shorter path's end the phase runs whole.
const (
	rsReceiveEvents   = 58
	rsRacingEvents    = 80
	rsRecoveryEvents  = 34
	rsTransportEvents = 14
)

func TestReceiveRandomStops(t *testing.T) { rsSeeds(t, false, rsRange(1, 96)) }

func TestReceiveRandomStopsRacing(t *testing.T) { rsSeeds(t, true, rsRange(1001, 32)) }

func TestReceiveRandomStopsSoak(t *testing.T) {
	if value := os.Getenv("OBERTH_RECEIVE_SEED"); value != "" {
		seed, err := strconv.ParseUint(strings.TrimPrefix(value, "race:"), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		rsSeeds(t, strings.HasPrefix(value, "race:"), []uint64{seed})
		return
	}
	value := os.Getenv("OBERTH_RECEIVE_SOAK")
	if value == "" {
		t.Skip("set OBERTH_RECEIVE_SOAK to a duration, or OBERTH_RECEIVE_SEED to replay one seed")
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		t.Fatal(err)
	}
	var raw [4]byte
	if _, err = crand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	base := uint64(binary.BigEndian.Uint32(raw[:])) << 8
	t.Logf("soak seeds start at %d", base)
	ctx, cancel := context.WithTimeout(t.Context(), duration)
	defer cancel()
	stats := &rsStats{}
	for batch := uint64(0); ctx.Err() == nil; batch++ {
		rsBatch(t, stats, batch%4 == 3, rsRange(base+batch*8, 8))
	}
	stats.log(t)
}

func rsRange(first uint64, count int) []uint64 {
	seeds := make([]uint64, count)
	for i := range seeds {
		seeds[i] = first + uint64(i)
	}
	return seeds
}

func rsSeeds(t *testing.T, race bool, seeds []uint64) {
	stats := &rsStats{}
	rsBatch(t, stats, race, seeds)
	stats.log(t)
	if len(seeds) > 1 {
		stats.exercised(t)
	}
}

func rsBatch(t *testing.T, stats *rsStats, race bool, seeds []uint64) {
	// Each scenario starts a server and its Git/hook process tree.
	slots := make(chan struct{}, 2)
	t.Run(fmt.Sprintf("from-%d", seeds[0]), func(t *testing.T) {
		for _, seed := range seeds {
			t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
				t.Parallel()
				slots <- struct{}{}
				t.Cleanup(func() { <-slots })
				stats.add(rsRunScenario(t, rsScenarioFor(seed, race)))
			})
		}
	})
}

type rsScenario struct {
	Seed      uint64
	Race      bool
	Shape     string
	Damage    string
	Refuse    bool
	AlwaysRun bool
	FailOnce  bool
	Transport string
	// FaultOnly applies Refuse and Transport to that racing push alone; -1
	// applies them to every push.
	FaultOnly int
	Stops     [2]rsStop
	// Survivors stops only the server: hooks run on as orphans.
	Survivors bool
	// Packed packs the initial refs, so deletions rewrite packed-refs.
	Packed bool
	// Debris leaves the temporary file a stop inside a record write leaves.
	Debris bool
}

// rsStop is no stop when zero, an ordinal on the main or the transport
// sequence, or a native rename.
type rsStop struct {
	Event     int    `json:",omitempty"`
	Transport int    `json:",omitempty"`
	Needle    string `json:",omitempty"`
	When      string `json:",omitempty"`
}

func rsPick(r *rand.Rand, options ...string) string { return options[r.IntN(len(options))] }

func rsScenarioFor(seed uint64, race bool) rsScenario {
	r := rand.New(rand.NewPCG(seed, 0x0be47))
	s := rsScenario{Seed: seed, Race: race, Shape: "update"}
	if !race {
		s.Shape = rsPick(r, "update", "create", "delete", "multi", "tag", "shape")
	}
	s.Damage = rsPick(r, "none", "none", "none", "none", "rt-bypassed", "rt-empty", "pre-bypassed")
	s.Refuse = r.IntN(7) == 0
	s.AlwaysRun = r.IntN(4) != 0
	s.FailOnce = r.IntN(4) == 0
	s.Transport = rsPick(r, "clean", "clean", "clean", "clean", "status-fail", "input-cut")
	events := rsReceiveEvents
	if race {
		events = rsRacingEvents
	}
	s.Stops[0] = rsStopFor(r, s.Shape, events, true)
	if r.IntN(2) == 0 {
		s.Stops[1] = rsStopFor(r, s.Shape, rsRecoveryEvents, false)
	}
	s.Survivors, s.Packed, s.Debris = r.IntN(3) == 0, r.IntN(3) == 0, r.IntN(3) == 0
	s.FaultOnly = -1
	if race {
		s.FaultOnly = r.IntN(3) - 1
	}
	if s.Shape == "delete" {
		// Admission refuses a run for a push that only deletes, so a delete
		// asks for a run rarely and mostly reaches the reference transaction.
		s.AlwaysRun = r.IntN(4) == 0
	}
	return s
}

// rsStopFor picks a main-sequence ordinal (a third of them in the later half
// of the path, after Git commits), a transport ordinal while a pusher is
// connected, or the first rename by any Git process onto a pushed reference,
// packed-refs or a retention ref (Git links objects).
func rsStopFor(r *rand.Rand, shape string, events int, transport bool) rsStop {
	switch r.IntN(7) {
	case 0:
	case 1:
		if transport {
			return rsStop{Transport: 1 + r.IntN(rsTransportEvents)}
		}
		return rsStop{Event: 1 + r.IntN(events)}
	case 2, 3:
		return rsStop{Event: events/2 + r.IntN(events-events/2) + 1}
	default:
		return rsStop{Event: 1 + r.IntN(events)}
	}
	needles := append(rsShapeRefs(shape), "packed-refs", "refs/oberth/keep/")
	return rsStop{Needle: rsPick(r, needles...), When: rsPick(r, "before", "after")}
}

func rsShapeRefs(shape string) []string {
	switch shape {
	case "create":
		return []string{"refs/heads/feature"}
	case "delete":
		return []string{"refs/heads/old"}
	case "multi":
		return []string{"refs/heads/topic", "refs/heads/feature", "refs/heads/old"}
	case "tag":
		return []string{"refs/tags/v1"}
	case "shape":
		return []string{"refs/heads/topic", "refs/heads/topic/leaf"}
	}
	return []string{"refs/heads/topic"}
}

// rsPushes are the pushes of a scenario: A is the initial commit, next and
// racing are new commits.
func rsPushes(s rsScenario, a, next, racing string) [][]gitcache.RefUpdate {
	u := func(ref, old, value string) gitcache.RefUpdate {
		return gitcache.RefUpdate{Ref: ref, OldSHA: old, NewSHA: value}
	}
	if s.Race {
		return [][]gitcache.RefUpdate{{u("refs/heads/topic", a, next)}, {u("refs/heads/topic", a, racing)}}
	}
	switch s.Shape {
	case "create":
		return [][]gitcache.RefUpdate{{u("refs/heads/feature", "", next)}}
	case "delete":
		return [][]gitcache.RefUpdate{{u("refs/heads/old", a, "")}}
	case "multi":
		return [][]gitcache.RefUpdate{{u("refs/heads/topic", a, next), u("refs/heads/feature", "", next), u("refs/heads/old", a, "")}}
	case "tag":
		return [][]gitcache.RefUpdate{{u("refs/tags/v1", "", next)}}
	case "shape":
		return [][]gitcache.RefUpdate{{u("refs/heads/topic", a, ""), u("refs/heads/topic/leaf", "", next)}}
	}
	return [][]gitcache.RefUpdate{{u("refs/heads/topic", a, next)}}
}

type rsRun struct {
	f       *gittest.Fixture
	sc      rsScenario
	control string
	cfg     gitcache.Config
	before  map[string]string
	pushes  [][]gitcache.RefUpdate
	inputs  []string
	follow  string
	phases  []rsPhase
}

type rsPhase struct {
	Stopped         bool
	Label           string
	Events          int
	TransportEvents int
	Delivered       bool
	Errs            []string
	Tail            string
}

func rsRunScenario(t *testing.T, sc rsScenario) rsResult {
	run := rsSetup(t, gittest.New(t), sc)
	run.phases = append(run.phases, run.phase(t, "receive", sc.Stops[0]))
	if sc.Stops[1] != (rsStop{}) {
		run.phases = append(run.phases, run.phase(t, "recover", sc.Stops[1]))
	}
	verdict, duplicates, leaked, problems := run.final(t)
	t.Logf("seed %d %+v: %s; receive keep refs left %d; phases %+v", sc.Seed, sc, verdict, leaked, run.phases)
	if len(problems) > 0 {
		data, _ := json.Marshal(sc)
		replay := strconv.FormatUint(sc.Seed, 10)
		if sc.Race {
			replay = "race:" + replay
		}
		t.Errorf("VIOLATION seed %d\nscenario %s\nphases %+v\nverdict %s\nproblems:\n  %s\n"+
			"replay: OBERTH_RECEIVE_SEED=%s go test -race -run '^TestReceiveRandomStopsSoak$' ./internal/gitcache/",
			sc.Seed, data, run.phases, verdict, strings.Join(problems, "\n  "), replay)
	}
	return rsResult{verdict: verdict, phases: run.phases, duplicates: duplicates, leaked: leaked}
}

func rsSetup(t *testing.T, f *gittest.Fixture, sc rsScenario) *rsRun {
	t.Helper()
	control := rsControl(t, f, sc.Stops[0].Needle != "" || sc.Stops[1].Needle != "")
	run := &rsRun{f: f, sc: sc, control: control, cfg: rsConfig(f, control)}
	c := rsOpen(t, run.cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	f.Git(t, f.Store(), "update-ref", "refs/heads/topic", f.Commit)
	f.Git(t, f.Store(), "update-ref", "refs/heads/old", f.Commit)
	if sc.Packed {
		f.Git(t, f.Store(), "pack-refs", "--all")
	}
	run.before = rsRefs(t, f)
	commits := make([]string, 3)
	for i, name := range []string{"next", "racing", "follow"} {
		f.Write(t, name+".txt", []byte(name+"\n"))
		commits[i] = f.CommitFile(t, name)
	}
	run.follow = commits[2]
	run.pushes = rsPushes(sc, f.Commit, commits[0], commits[1])
	for i, push := range run.pushes {
		data, err := io.ReadAll(receiveBatchInput(t, f, push, nil))
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(control, "input-"+strconv.Itoa(i))
		if err = os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		run.inputs = append(run.inputs, path)
	}
	if sc.Damage != "none" {
		rsWrite(t, filepath.Join(control, "damage"), sc.Damage)
	}
	if sc.FailOnce {
		rsWrite(t, filepath.Join(control, "fail-once"), "")
	}
	if sc.Survivors {
		rsWrite(t, filepath.Join(control, "survivors"), "")
	}
	return run
}

// rsQuiesce waits until no process of the stopped instance (an orphaned
// hook, say) still holds the root, so the next phase's ordinals are its own.
func (r *rsRun) quiesce(t *testing.T) {
	t.Helper()
	owner, err := os.OpenFile(filepath.Join(r.cfg.Root, "oberth.owner"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for syscall.Flock(int(owner.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		select {
		case <-deadline.C:
			t.Fatal("processes of the stopped server still hold its root")
		case <-tick.C:
		}
	}
}

// rsControl writes the run's wrappers, killer and (for native stops) the
// package's rename fault library.
func rsControl(t *testing.T, f *gittest.Fixture, native bool) string {
	t.Helper()
	control := filepath.Join(f.Root, "ctl")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"git": rsGitWrapper(control, f.Binary), "hook": rsHookWrapper(control, binary), "killer.py": rsKiller}
	if native {
		image, err := nativeFaultImage()
		if err != nil {
			t.Fatal(err)
		}
		files["fault.so"] = string(image)
	}
	if err = os.Mkdir(control, 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err = os.WriteFile(filepath.Join(control, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return control
}

func rsRemove(t *testing.T, control string, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := os.Remove(filepath.Join(control, name)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

func rsConfig(f *gittest.Fixture, control string) gitcache.Config {
	return gitcache.Config{
		Root: filepath.Join(f.Root, "cache"), Scratch: filepath.Join(f.Root, "scratch"),
		Binary: filepath.Join(control, "git"), HookBinary: filepath.Join(control, "hook"),
		Upstream: func(context.Context, string) (gitcache.Remote, error) {
			return gitcache.Remote{URL: f.Bare, DefaultBranch: "refs/heads/main", RepositoryID: 1}, nil
		},
		LiveOwners: func(context.Context, string) (gitcache.OwnerPage, error) { return gitcache.OwnerPage{}, nil },
		PreReceive: func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error { return nil },
	}
}

// phase runs one server process with the stop armed and reports where it
// stopped, from the outside: its exit signal and the shared event log.
func (r *rsRun) phase(t *testing.T, mode string, stop rsStop) rsPhase {
	t.Helper()
	rsRemove(t, r.control, "events", "target", "native", "native-stop", "result", "server.pid")
	rsWrite(t, filepath.Join(r.control, "seq"), "0")
	rsWrite(t, filepath.Join(r.control, rsTransportStream+"seq"), "0")
	if stop.Needle != "" {
		rsWrite(t, filepath.Join(r.control, "native"), stop.Needle+" "+stop.When)
	}
	spec := rsSpec{Root: r.cfg.Root, Scratch: r.cfg.Scratch, Binary: r.cfg.Binary, Hook: r.cfg.HookBinary,
		Bare: r.f.Bare, Control: r.control, Mode: mode, Commit: r.f.Commit, Target: stop.Event, TransportTarget: stop.Transport}
	if mode == "receive" {
		spec.Inputs, spec.Refuse, spec.AlwaysRun, spec.Transport = r.inputs, r.sc.Refuse, r.sc.AlwaysRun, r.sc.Transport
		spec.FaultOnly = r.sc.FaultOnly
		for _, push := range r.pushes {
			spec.Values = append(spec.Values, push[0].NewSHA)
		}
	}
	output, err := r.spawn(t, spec)
	r.quiesce(t)
	var exit *exec.ExitError
	phase := rsPhase{Stopped: errors.As(err, &exit) && exit.Sys().(syscall.WaitStatus).Signal() == syscall.SIGKILL}
	if err != nil && !phase.Stopped {
		t.Fatalf("server process failed outside a stop: %v\n%s", err, output)
	}
	r.readEvents(t, &phase, stop)
	if !phase.Stopped {
		data, err := os.ReadFile(filepath.Join(r.control, "result"))
		if err != nil || json.Unmarshal(data, &phase.Errs) != nil {
			t.Fatalf("server process left no result: %v\n%s", err, output)
		}
	}
	rsRemove(t, r.control, "target", "native", "damage")
	return phase
}

func (r *rsRun) spawn(t *testing.T, spec rsSpec) (string, error) {
	t.Helper()
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	cmd := exec.Command("/usr/bin/python3", "-c", rsReaper, binary, "-test.run=^TestReceiveRandomStopWorker$", "-test.count=1")
	cmd.Env = append(os.Environ(), "OBERTH_RANDOM_STOP="+string(data), "GORACE=atexit_sleep_ms=0")
	cmd.Stdout, cmd.Stderr, cmd.WaitDelay = &output, &output, 10*time.Second
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	killed := false
	for {
		select {
		case err = <-done:
			return output.String(), err
		case <-tick.C:
			if _, statErr := os.Stat(filepath.Join(r.control, "native-stop")); statErr == nil && !killed {
				killed = true
				r.kill()
			}
		case <-deadline.C:
			r.kill()
			<-done
			t.Fatalf("server process hung\n%s", output.String())
		}
	}
}

func (r *rsRun) kill() {
	_ = exec.Command("/usr/bin/python3", filepath.Join(r.control, "killer.py"), r.control).Run()
}

func rsRefs(t *testing.T, f *gittest.Fixture) map[string]string {
	t.Helper()
	refs := make(map[string]string)
	out := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads", "refs/tags")
	for _, line := range strings.Split(out, "\n") {
		if name, oid, ok := strings.Cut(line, " "); ok {
			refs[name] = oid
		}
	}
	return refs
}
