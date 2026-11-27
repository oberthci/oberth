package sourcedelivery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/sourcecontract"
	core "k8s.io/api/core/v1"
)

// Caller is returned only after bearer verification and a live Pod-owner read.
type Caller struct {
	Namespace, Workflow, UID string
	Pod                      *core.Pod
}

func (c Caller) owner() Caller { c.Pod = nil; return c }

// Run carries the stored admission projection, without constructing a new seal.
// Terminal becomes true only when loading must stop; the owner reader retains
// refs through all native exit handlers.
type Run struct {
	Locator                  string
	caller                   Caller
	Admission                admissioncontract.PreparedView
	Namespace, Workflow, UID string
	Terminal                 bool
}
type Staged struct {
	Body  io.ReadCloser
	Bytes int64
	// Waited reports any capacity, repository or recovery wait inside Stage,
	// before any response bytes. It belongs to this load, including on a hit.
	Waited bool
}
type Config struct {
	Authenticate func(context.Context, string) (Caller, error)
	Run          func(context.Context, string) (Run, error)
	Stage        func(context.Context, sourcecontract.Binding) (Staged, error)
	// Metric receives exactly one final summary per handled request. It must not block or do I/O.
	Metric        func(Summary)
	Permits       *Permits
	Budgets       *Budgets
	Now           func() time.Time
	RecordFailure func(context.Context, Caller, time.Time) error
	RecordSuccess func(context.Context, Caller, time.Time) error
}
type Server struct {
	config          Config
	budgets         Budgets
	valid           bool
	auth, transfers chan struct{}
	mu              sync.Mutex
	workflows       map[Caller]int
	waiters         map[Caller]int
}

// Failure contains only a protocol code and fixed capacity reason.
type Failure struct {
	Code, Reason, Message, Unit string
	Limit                       int64
}

func (e *Failure) Error() string {
	switch e.Code {
	case "too_large":
		return reasonTooLarge
	case "gone":
		if e.Message != "" {
			return e.Message
		}
		return reasonGone
	case "unauthenticated":
		return reasonUnauthenticated
	case "forbidden":
		return reasonForbidden
	case "not_configured":
		return reasonUnconfigured
	case "invalid_request":
		return reasonRequest
	}
	return reasonUnavailable
}
func (e *Failure) Remedy() string {
	switch e.Code {
	case "too_large":
		return remedyTooLarge
	case "gone":
		return remedyGone
	default:
		return remedyRequest
	}
}

func New(config Config) *Server {
	b, err := ResolveBudgets(config.Budgets)
	if err != nil {
		b = DefaultBudgets()
	}
	return &Server{config: config, budgets: b, valid: err == nil, auth: make(chan struct{}, b.Auth), transfers: make(chan struct{}, b.Transfers), workflows: make(map[Caller]int), waiters: make(map[Caller]int)}
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	observation := newObservation("load")
	ctx := context.WithValue(r.Context(), observationContext{}, observation)
	result := &outcomeWriter{ResponseWriter: w, code: "ok", observation: observation}
	w = result
	defer func() {
		if s.config.Metric != nil {
			summary := observation.Finish(result.code, result.err)
			summary.Bytes, summary.Reason = result.bytes, result.reason
			s.config.Metric(summary)
		}
	}()
	if r.URL.Path != LoadPath {
		outcome(w, "invalid_request")
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		outcome(w, "invalid_request")
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(405)
		return
	}
	if !s.valid || s.config.Authenticate == nil || s.config.Run == nil || s.config.Stage == nil || s.config.Now == nil {
		s.fail(w, 501, "not_configured", "")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	request, err := s.request(ctx, w, r)
	if err != nil {
		outcomeError(w, err)
		s.fail(w, 400, "invalid_request", "")
		return
	}
	s.load(ctx, w, r, request)
	if result.err == nil {
		result.err = ctx.Err()
	}
}

func (s *Server) load(ctx context.Context, w http.ResponseWriter, r *http.Request, request LoadRequest) {
	started := s.config.Now()
	run, binding, index, err := s.authorize(ctx, r.Header.Get("Authorization"), request)
	if err != nil {
		s.failure(w, err)
		return
	}
	waited, err := s.transferSlot(ctx, Caller{Namespace: run.Namespace, Workflow: run.Workflow, UID: run.UID})
	if err != nil {
		s.failure(w, err)
		return
	}
	defer func() { <-s.transfers }()
	defer s.config.Permits.Enter(Transfer)()
	if waited {
		// A request waiting for capacity cannot use its old Pod/run decision.
		run, binding, index, err = s.authorize(ctx, r.Header.Get("Authorization"), request)
		if err != nil {
			s.failure(w, err)
			return
		}
	}
	staged, err := s.config.Stage(ctx, binding)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = &terminalFailure{s.recordFailure(ctx, run.caller, started, err)}
		}
		s.failure(w, err)
		return
	}
	load := transfer{request: request, admission: run.Admission.Digest, index: index, stage: staged}
	load.stage.Body = &failureReader{ReadCloser: staged.Body, fail: func(err error) error { return s.recordFailure(ctx, run.caller, started, err) }}
	load.success = func() error {
		if s.config.RecordSuccess != nil {
			return s.config.RecordSuccess(ctx, run.caller, started)
		}
		return nil
	}
	if err := s.checkStagedAuthorization(ctx, r.Header.Get("Authorization"), load); err != nil {
		_ = staged.Body.Close()
		s.failure(w, err)
		return
	}
	s.send(ctx, w, load)
}

func (s *Server) transferSlot(ctx context.Context, caller Caller) (bool, error) {
	if acquire(s.transfers) {
		return false, nil
	}
	defer Measure(ctx, Transfer, true)()
	if !s.waiter(caller, 1) {
		return false, &Failure{Code: "unavailable", Reason: "transfer-slot"}
	}
	defer s.waiter(caller, -1)
	select {
	case s.transfers <- struct{}{}:
		return true, nil
	case <-ctx.Done():
		return true, context.Cause(ctx)
	}
}

func (s *Server) checkStagedAuthorization(ctx context.Context, header string, load transfer) error {
	if !load.stage.Waited {
		return nil
	}
	fresh, _, _, err := s.authorize(ctx, header, load.request)
	if err != nil {
		return err
	}
	if fresh.Admission.Digest != load.admission {
		return &Failure{Code: "forbidden"}
	}
	return nil
}

func (s *Server) waiter(c Caller, delta int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if delta > 0 && s.waiters[c] >= s.budgets.WorkflowWaiters {
		return false
	}
	s.waiters[c] += delta
	if s.waiters[c] == 0 {
		delete(s.waiters, c)
	}
	return true
}
func (s *Server) authorize(ctx context.Context, header string, request LoadRequest) (Run, sourcecontract.Binding, int, error) {
	defer Measure(ctx, Auth, false)()
	empty := sourcecontract.Binding{}
	if !strings.HasPrefix(header, "Bearer ") || len(header) > 8192 || len(header) <= 7 {
		return Run{}, empty, 0, &Failure{Code: "unauthenticated"}
	}
	if !acquire(s.auth) {
		return Run{}, empty, 0, &Failure{Code: "unavailable", Reason: "auth-cap"}
	}
	defer func() { <-s.auth }()
	defer s.config.Permits.Enter(Auth)()
	phase, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	caller, err := s.config.Authenticate(phase, strings.TrimPrefix(header, "Bearer "))
	if err != nil {
		return Run{}, empty, 0, err
	}

	if caller.UID == "" || caller.Workflow == "" || caller.Namespace == "" {
		return Run{}, empty, 0, &Failure{Code: "unauthenticated"}
	}
	if !s.workflow(caller, 1) {
		return Run{}, empty, 0, &Failure{Code: "unavailable", Reason: "auth-cap"}
	}
	defer s.workflow(caller, -1)
	id := runID(caller, request.Locator)
	started := s.config.Now()
	run, err := s.config.Run(phase, id)
	if err != nil {
		// Immediate database errors remain retryable 503s. Only an exhausted
		// server delivery phase is terminal and may create a receipt.
		if errors.Is(phase.Err(), context.DeadlineExceeded) {
			err = &terminalFailure{s.recordFailure(ctx, caller, started, err)}
		}
		return run, empty, 0, err
	}
	run.caller = caller
	binding, index, err := selectBinding(run, caller, LoadRequest{Locator: id, Descriptor: request.Descriptor})
	return run, binding, index, err
}

// runID is the run a locator names. A schedule child's run ID is its own
// Workflow name, so the scheduled locator names the caller's own run; the
// recorded Workflow name and UID are still checked against the caller.
func runID(caller Caller, locator string) string {
	if locator == sourcecontract.ScheduledLocator {
		return caller.Workflow
	}
	return locator
}
func selectBinding(run Run, caller Caller, request LoadRequest) (sourcecontract.Binding, int, error) {
	empty := sourcecontract.Binding{}
	if run.Locator != "" && run.Locator != run.Admission.RunID && caller.Pod != nil && caller.Pod.Status.Phase != core.PodPending && caller.Pod.Status.Phase != core.PodRunning {
		return empty, 0, &Failure{Code: "forbidden"}
	}
	if run.Terminal {
		return empty, 0, &Failure{Code: "forbidden"}
	}
	if run.UID == "" {
		return empty, 0, &Failure{Code: "unavailable", Reason: "other"}
	}
	if caller.owner() != (Caller{Namespace: run.Namespace, Workflow: run.Workflow, UID: run.UID}) {
		return empty, 0, &Failure{Code: "forbidden"}
	}
	view := run.Admission
	locator := run.Locator
	if locator == "" {
		locator = view.RunID
	}
	if locator != request.Locator || view.Digest == "" {
		return empty, 0, &Failure{Code: "forbidden"}
	}
	return findBinding(view.SourceBindings, request.Descriptor)
}
func findBinding(bindings []sourcecontract.Binding, descriptor sourcecontract.Descriptor) (sourcecontract.Binding, int, error) {
	pins := make(map[sourcecontract.Descriptor]int)
	for _, b := range bindings {
		index := 0
		if b.Descriptor.Kind == sourcecontract.Pinned {
			var exists bool
			index, exists = pins[b.Descriptor]
			if !exists {
				index = len(pins)
				pins[b.Descriptor] = index
			}
		}
		if b.Descriptor == descriptor {
			return b, index, nil
		}
	}
	return sourcecontract.Binding{}, 0, &Failure{Code: "forbidden"}
}

func (s *Server) workflow(c Caller, delta int) bool {
	c = c.owner()
	s.mu.Lock()
	defer s.mu.Unlock()
	if delta > 0 && s.workflows[c] >= 2 {
		return false
	}
	s.workflows[c] += delta
	if s.workflows[c] == 0 {
		delete(s.workflows, c)
	}
	return true
}
func acquire(ch chan struct{}) bool {
	select {
	case ch <- struct{}{}:
		return true
	default:
		return false
	}
}

type transfer struct {
	request   LoadRequest
	admission string
	index     int
	stage     Staged
	success   func() error
}

func (s *Server) send(ctx context.Context, w http.ResponseWriter, t transfer) {
	r, admission, index, stage := t.request, t.admission, t.index, t.stage
	defer Measure(ctx, Transfer, false)()
	closed := false
	defer func() {
		if !closed {
			_ = stage.Body.Close()
		}
	}()
	w.Header().Set("Content-Type", LoadContentType)
	w.Header().Set("Trailer", ReceiptTrailer)
	digest := sha256.New()
	writer := &deadlineWriter{ctx: ctx, w: w, now: s.config.Now}
	if _, err := io.Copy(io.MultiWriter(writer, digest), stage.Body); err != nil {
		outcomeError(w, err)
		outcome(w, "unavailable")
		return
	}
	if ctx.Err() != nil {
		outcomeError(w, ctx.Err())
		outcome(w, "unavailable")
		return
	}
	closed = true
	if err := stage.Body.Close(); err != nil {
		outcomeError(w, err)
		outcome(w, "unavailable")
		return
	}
	receipt := LoadReceipt{Version: 1, Locator: r.Locator, Descriptor: r.Descriptor, Stream: StreamReceipt{AdmissionDigest: admission, Payload: Payload{Kind: r.Descriptor.Kind, Index: &index}, Digest: hex.EncodeToString(digest.Sum(nil)), Bytes: &stage.Bytes}}
	value, err := EncodeReceipt(receipt)
	if err != nil {
		outcome(w, "unavailable")
		return
	}
	if t.success != nil && t.success() != nil {
		outcome(w, "unavailable")
	}
	w.Header().Set(ReceiptTrailer, value)
}

type deadlineWriter struct {
	ctx context.Context
	w   http.ResponseWriter
	now func() time.Time
}

func (d *deadlineWriter) Write(b []byte) (int, error) {
	if d.ctx.Err() != nil {
		return 0, context.Cause(d.ctx)
	}
	err := http.NewResponseController(d.w).SetWriteDeadline(boundedDeadline(d.ctx, d.now().Add(time.Minute)))
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		return 0, err
	}
	return d.w.Write(b)
}
func (s *Server) failure(w http.ResponseWriter, err error) {
	outcomeError(w, err)
	outcome(w, "unavailable")
	var terminal *terminalFailure
	if errors.As(err, &terminal) {
		outcome(w, "unavailable")
		s.fail(w, 502, "delivery_failed", "")
		return
	}
	var f *Failure
	if !errors.As(err, &f) {
		s.fail(w, 503, "unavailable", "other")
		return
	}
	switch f.Code {
	case "too_large", "gone":
		outcome(w, f.Code)
		status := 413
		if f.Code == "gone" {
			status = 410
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(ErrorResponse{Version: 1, Code: f.Code, Limit: f.Limit, Message: f.Message, Unit: f.Unit})
	case "forbidden":
		s.fail(w, 403, f.Code, "")
	case "unauthenticated":
		s.fail(w, 401, f.Code, "")
	default:
		s.fail(w, 503, "unavailable", f.Reason)
	}
}
func (s *Server) fail(w http.ResponseWriter, status int, code, reason string) {
	if code == "unavailable" {
		switch reason {
		case "export-slot", "auth-cap", "transfer-slot":
			w.Header().Set("Retry-After", "1")
		default:
			reason = "other"
			w.Header().Set("Retry-After", "10")
		}
	}
	outcome(w, code)
	if result, ok := w.(*outcomeWriter); ok {
		result.reason = reason
	}
	if result, ok := w.(*outcomeWriter); ok && code == "unavailable" {
		switch reason {
		case "auth-cap":
			result.block(Auth)
		case "export-slot":
			result.block(Export)
		case "transfer-slot":
			result.block(Transfer)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorResponse{Version: 1, Code: code})
}
func (s *Server) request(ctx context.Context, w http.ResponseWriter, r *http.Request) (LoadRequest, error) {
	err := http.NewResponseController(w).SetReadDeadline(boundedDeadline(ctx, s.config.Now().Add(10*time.Second)))
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		return LoadRequest{}, err
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, MaxLoadRequestBytes+1))
	if err != nil {
		return LoadRequest{}, err
	}
	return DecodeRequest(data)
}
func boundedDeadline(ctx context.Context, proposed time.Time) time.Time {
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(proposed) {
		return deadline
	}
	return proposed
}
