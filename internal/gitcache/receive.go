package gitcache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/oberthci/oberth/internal/gitoid"
	"github.com/oberthci/oberth/internal/reponame"
	"github.com/oberthci/oberth/internal/strictjson"
)

const MaxUpdates = 256
const maxReceiveRecordBytes = 8 << 20

type RefUpdate struct {
	Ref    string `json:"ref"`
	OldSHA string `json:"old_sha"`
	NewSHA string `json:"new_sha"`
}
type ReleaseAdmission struct {
	DefaultBranch string `json:"default_branch"`
	SHA           string `json:"sha"`
}
type ReceiveEvent struct {
	ID          string           `json:"id"`
	Actor       string           `json:"actor"`
	Fingerprint string           `json:"fingerprint"`
	Repository  string           `json:"repository"`
	Updates     []RefUpdate      `json:"updates"`
	Options     PushOptions      `json:"options"`
	Admission   ReleaseAdmission `json:"admission"`
}
type Reservation struct {
	Problem          string              `json:"-"`
	Prepared         map[string]struct{} `json:"prepared"`
	Before           map[string]string   `json:"before"`
	ReservedBranches []string            `json:"reserved_branches"`
	AllowDefault     bool                `json:"allow_default"`
	AlwaysRun        bool                `json:"always_run"`
	State            string              `json:"state"`
	Event            ReceiveEvent        `json:"event"`
}

// ReceiveRequest carries authenticated server metadata, never client options.
// Begin/End bracket client transport for the SSH stream inactivity bound.
type ReceiveRequest struct {
	Repository, Actor, Fingerprint string
	Admission                      ReleaseAdmission
	// AllowDefault is supplied only by authenticated operator admission.
	AllowDefault        bool
	AlwaysRun           bool
	Input               io.ReadCloser
	Output, Diagnostics io.Writer
	Begin               func()
	End                 func()
	// Deliver's nil error acknowledges durably saved runs. Its lines are then
	// printed to the pusher; an error retains the replay record and roots.
	Deliver func(context.Context, ReceiveEvent) ([]string, error)
}

// Receive stores original attribution before Git can change any ref. A failed
// durable delivery remains ready for replay with its stable event ID.
func (c *Cache) Receive(ctx context.Context, r ReceiveRequest) (result error) {
	ctx, observation := sourcedelivery.Observe(ctx, "receive")
	defer func() { c.observe(observation, result) }()
	p, err := c.path(r.Repository)
	if err != nil {
		return err
	}
	if r.Actor == "" || r.Fingerprint == "" || r.Input == nil || !filepath.IsAbs(c.config.HookBinary) {
		return refuse("receive identity")
	}
	if err = c.checkHook(); err != nil {
		return err
	}
	if err = validateAdmission(r.Admission); err != nil {
		return err
	}
	if err = acquireReceive(ctx, c.repositoryGate(p+"/receive"), r.Diagnostics); err != nil {
		return err
	}
	defer func() { <-c.repositoryGate(p + "/receive") }()
	if err = c.beforeReceive(ctx, p, r); err != nil {
		return c.reportReceiveFailure(r.Diagnostics, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.receiveTransport(ctx, p, r)
}

func acquireReceive(ctx context.Context, slot chan struct{}, diagnostics io.Writer) error {
	select {
	case slot <- struct{}{}:
		return nil
	default:
	}
	if err := writeFeedback(diagnostics, []string{reasonPushWaiting + ". " + remedyPushWaiting}); err != nil {
		return err
	}
	defer sourcedelivery.Measure(ctx, sourcedelivery.ReceiveSlot, true)()
	return acquireContext(ctx, slot)
}

func (c *Cache) beforeReceive(ctx context.Context, p string, r ReceiveRequest) error {
	c.activeMu.Lock()
	blocked := c.replayProblems[p] != ""
	c.activeMu.Unlock()
	if blocked {
		return pushBusy(r.Repository)
	}
	_, err := c.replay(ctx, p, r.Deliver)
	if err == nil {
		return nil
	}
	c.recordReplayProblem(p, err)
	if c.config.Log != nil {
		c.config.Log(reasonPushBusy, err.Error())
	}
	var delivery *deliveryPendingError
	var cleanup *receiveCleanupPendingError
	if errors.As(err, &delivery) || errors.As(err, &cleanup) {
		return pushBusy(r.Repository)
	}
	return err
}

type receiveDelivery struct {
	passed  string
	reached bool
}

func (c *Cache) abortUndelivered(d *receiveDelivery) {
	if d.passed != "" && !d.reached && c.config.AbortReceive != nil {
		c.config.AbortReceive(d.passed)
	}
}
func (c *Cache) deliverPush(ctx context.Context, p string, r ReceiveRequest, delivery *receiveDelivery) error {
	deliver := r.Deliver
	if deliver != nil {
		deliver = func(ctx context.Context, e ReceiveEvent) ([]string, error) {
			if e.ID == delivery.passed {
				delivery.reached = true
			}
			return r.Deliver(ctx, e)
		}
	}
	lines, err := c.replay(ctx, p, deliver)
	return errors.Join(err, writeFeedback(r.Diagnostics, lines))
}
func (c *Cache) receiveTransport(ctx context.Context, p string, r ReceiveRequest) (err error) {
	owner, err := c.reserveReceive(ctx, p, r)
	if err != nil {
		return err
	}
	defer c.setActive(owner, false)
	ctx, cancelNative := context.WithCancelCause(ctx)
	defer cancelNative(nil)
	progress := watchProgress(cancelNative, c.budgets.idle)
	defer progress.close()
	activity := &receiveProgress{progress: progress, begin: r.Begin, end: r.End}
	r.Output, r.Diagnostics = activity.writer(r.Output), activity.writer(r.Diagnostics)
	owned := c.trackReceiveFiles(p, owner)
	result := receiveCompletion{owner: owner}
	defer func() {
		result.err = errors.Join(result.err, context.Cause(ctx))
		if result.gate != nil {
			_ = result.gate.close()
		}
		if result.failed() {
			owned.captureQuarantines(p)
		}
		result.err = errors.Join(result.err, c.completeReceive(ctx, p, owned, &result))
		c.setActive(owner, false)
		err = c.finishTransport(ctx, p, r, result)
	}()
	result.gate, result.err = c.startGate(ctx, gateSession{store: p, owner: owner, progress: activity})
	if result.err == nil {
		result.err = c.runReceive(ctx, r, command{
			dir: p, args: []string{"-c", "receive.advertisePushOptions=true", "receive-pack", "."},
			input: r.Input, output: r.Output, diagnostics: r.Diagnostics,
			hooks: true, progress: progress,
			env: []string{"OBERTH_GIT_BINARY=" + c.config.Binary, "OBERTH_RECEIVE_GATE=" + result.gate.address},
		})
	}
	result.returned = true
	return nil
}

// Completion delivers accepted refs before releasing the receive gate, including
// while an original transport panic is unwinding.
func (c *Cache) finishTransport(ctx context.Context, p string, r ReceiveRequest, result receiveCompletion) error {
	passed, err := c.receiveFeedback(result, r)
	delivery := receiveDelivery{passed: passed}
	defer c.abortUndelivered(&delivery)
	var cleanup *receiveCleanupPendingError
	if errors.As(result.err, &cleanup) {
		return err
	}
	if err == nil || result.accepted {
		err = errors.Join(err, c.reportReceiveFailure(r.Diagnostics, c.deliverPush(ctx, p, r, &delivery)))
	}
	return err
}

func (c *Cache) runReceive(ctx context.Context, r ReceiveRequest, transport command) error {
	stop := context.AfterFunc(ctx, func() { _ = r.Input.Close() })
	defer stop()
	defer func() {
		if r.End != nil {
			r.End()
		}
		if c.receiveReturned != nil {
			c.receiveReturned()
		}
	}()
	if r.Begin != nil {
		r.Begin()
	}
	return c.runBoundedReceive(ctx, r, transport)
}

func writeFeedback(w io.Writer, lines []string) error {
	if w == nil {
		return nil
	}
	for _, line := range lines {
		if _, err := io.WriteString(w, line+"\n"); err != nil {
			return err
		}
	}
	return nil
}
func (c *Cache) reserveReceive(ctx context.Context, p string, r ReceiveRequest) (string, error) {
	if err := c.lock(ctx, p); err != nil {
		return "", err
	}
	defer c.unlock(p)
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return "", err
	}
	if err := c.checkHook(); err != nil {
		return "", err
	}
	if err := c.installHook(p); err != nil {
		return "", hookUnavailable(err)
	}
	before, err := c.refInventory(ctx, p)
	if err != nil {
		return "", err
	}
	record := Reservation{
		Before:           before,
		State:            "reserved",
		AllowDefault:     r.AllowDefault,
		AlwaysRun:        r.AlwaysRun,
		ReservedBranches: c.config.ReservedBranches,
		Event: ReceiveEvent{
			ID:          hex.EncodeToString(id),
			Actor:       r.Actor,
			Fingerprint: r.Fingerprint,
			Repository:  r.Repository,
			Admission:   r.Admission,
		},
	}
	if err := writeReservation(p, record); err != nil {
		return "", err
	}
	if r.Admission.SHA != "" {
		if err := c.keep(ctx, p, "receive-"+record.Event.ID, []string{r.Admission.SHA}); err != nil {
			return "", err
		}
	}
	owner := "receive-" + record.Event.ID
	c.setActive(owner, true)
	return owner, nil
}
func acquireContext(ctx context.Context, ch chan struct{}) error {
	select {
	case ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func validateAdmission(r ReleaseAdmission) error {
	if sourcecontract.ValidatePublicRef(r.DefaultBranch) != nil || !strings.HasPrefix(r.DefaultBranch, "refs/heads/") {
		return refuse("release admission")
	}
	if r.SHA != "" && !gitoid.Valid(r.SHA) {
		return refuse("release commit")
	}
	return nil
}
func writeReservation(p string, r Reservation) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return writeDurable(filepath.Join(p, receiveRecordFile), data)
}
func readReservationData(p string) ([]byte, error) {
	f, err := os.Open(filepath.Join(p, receiveRecordFile))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, maxReceiveRecordBytes+1))
}

func readReservation(p string) (Reservation, error) {
	data, err := readReservationData(p)
	if err != nil {
		return Reservation{}, err
	}
	var r Reservation
	if err = strictjson.DecodeStrictObject(data, &r, maxReceiveRecordBytes); err != nil {
		return r, &CorruptReceiveError{Cause: err}
	}
	if r.Event.ID == "" || r.Event.Actor == "" || r.Event.Fingerprint == "" {
		return r, &CorruptReceiveError{Cause: refuse("receive record")}
	}
	switch r.State {
	case "reserved", "ready", "rejected":
	default:
		return r, &CorruptReceiveError{Cause: refuse("receive state")}
	}
	if err = validateReservation(p, r); err != nil {
		return r, &CorruptReceiveError{Cause: err}
	}
	return r, nil
}

func validateReservation(p string, r Reservation) error {
	if r.Before == nil {
		return refuse("receive predecessors")
	}
	if !validReceiveID(r.Event.ID) {
		return refuse("receive record ID")
	}
	repo, err := reponame.ParseRepository(r.Event.Repository)
	if err != nil {
		return refuse("receive repository")
	}
	suffix := filepath.Join(repo.Upstream, repo.Organization, repo.Name+".git")
	if !strings.HasSuffix(p, string(filepath.Separator)+suffix) {
		return refuse("receive repository identity")
	}
	if len(r.Event.Updates) > MaxUpdates {
		return refuse("receive update count")
	}
	if err = validatePatterns(r.ReservedBranches); err != nil {
		return err
	}
	if err = validateRecordUpdates(r.Event.Updates); err != nil {
		return err
	}
	return validateAdmission(r.Event.Admission)
}

func validateRecordUpdates(updates []RefUpdate) error {
	seen := make(map[string]bool)
	for _, u := range updates {
		if seen[u.Ref] || sourcecontract.ValidatePublicRef(u.Ref) != nil {
			return refuse("receive update ref")
		}
		seen[u.Ref] = true
		if !optionalObject(u.OldSHA) || !optionalObject(u.NewSHA) {
			return refuse("receive update object")
		}
	}
	return nil
}

type CorruptReceiveError struct{ Cause error }

func (e *CorruptReceiveError) Error() string { return e.Reason() + ". " + e.Remedy() }
func (*CorruptReceiveError) Reason() string  { return reasonCorrupt }
func (*CorruptReceiveError) Remedy() string  { return remedyCorrupt }
func (e *CorruptReceiveError) Unwrap() error { return e.Cause }

func validReceiveID(value string) bool {
	id, err := hex.DecodeString(value)
	return err == nil && len(id) == 16 && hex.EncodeToString(id) == value
}
