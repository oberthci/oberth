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
)

// Caller is returned only after bearer verification and a live Pod-owner read.
type Caller struct{ Namespace, Workflow, UID string }

// Run carries the stored admission projection, without constructing a new seal.
// Terminal becomes true only when loading must stop; the owner reader retains
// refs through all native exit handlers.
type Run struct {
	Admission                admissioncontract.PreparedView
	Namespace, Workflow, UID string
	Terminal                 bool
}
type Staged struct {
	Body  io.ReadCloser
	Bytes int64
}
type Config struct {
	Authenticate      func(context.Context, string) (Caller, error)
	Run               func(context.Context, string) (Run, error)
	Stage             func(context.Context, sourcecontract.Binding) (Staged, error)
	Metric            func(code, reason string)
	SourceClientBurst int
	Now               func() time.Time
}
type Server struct {
	config          Config
	auth, transfers chan struct{}
	mu              sync.Mutex
	workflows       map[Caller]int
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
	return &Server{config: config, auth: make(chan struct{}, max(0, config.SourceClientBurst/3)), transfers: make(chan struct{}, 2), workflows: make(map[Caller]int)}
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != LoadPath {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(405)
		return
	}
	if s.config.Authenticate == nil || s.config.Run == nil || s.config.Stage == nil || s.config.Now == nil {
		s.fail(w, 501, "not_configured", "")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	request, err := s.request(ctx, w, r)
	if err != nil {
		s.fail(w, 400, "invalid_request", "")
		return
	}
	run, binding, index, err := s.authorize(ctx, r.Header.Get("Authorization"), request)
	if err != nil {
		s.failure(w, err)
		return
	}
	if !acquire(s.transfers) {
		s.fail(w, 503, "unavailable", "transfer-slot")
		return
	}
	defer func() { <-s.transfers }()
	staged, err := s.config.Stage(ctx, binding)
	if err != nil {
		s.failure(w, err)
		return
	}
	s.send(ctx, w, transfer{request: request, admission: run.Admission.Digest, index: index, stage: staged})
}
func (s *Server) authorize(ctx context.Context, header string, request LoadRequest) (Run, sourcecontract.Binding, int, error) {
	empty := sourcecontract.Binding{}
	if !strings.HasPrefix(header, "Bearer ") || len(header) > 8192 || len(header) <= 7 {
		return Run{}, empty, 0, &Failure{Code: "unauthenticated"}
	}
	if !acquire(s.auth) {
		return Run{}, empty, 0, &Failure{Code: "unavailable", Reason: "auth-cap"}
	}
	defer func() { <-s.auth }()
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
	run, err := s.config.Run(phase, id)
	if err != nil {
		return run, empty, 0, err
	}
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
	if run.Terminal {
		return empty, 0, &Failure{Code: "forbidden"}
	}
	if run.UID == "" {
		return empty, 0, &Failure{Code: "unavailable", Reason: "other"}
	}
	if caller != (Caller{Namespace: run.Namespace, Workflow: run.Workflow, UID: run.UID}) {
		return empty, 0, &Failure{Code: "forbidden"}
	}
	view := run.Admission
	if view.RunID != request.Locator || view.Digest == "" {
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
}

func (s *Server) send(ctx context.Context, w http.ResponseWriter, t transfer) {
	r, admission, index, stage := t.request, t.admission, t.index, t.stage
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
		s.metric("unavailable", "other")
		return
	}
	if ctx.Err() != nil {
		s.metric("unavailable", "other")
		return
	}
	closed = true
	if err := stage.Body.Close(); err != nil {
		s.metric("unavailable", "other")
		return
	}
	receipt := LoadReceipt{Version: 1, Locator: r.Locator, Descriptor: r.Descriptor, Stream: StreamReceipt{AdmissionDigest: admission, Payload: Payload{Kind: r.Descriptor.Kind, Index: &index}, Digest: hex.EncodeToString(digest.Sum(nil)), Bytes: &stage.Bytes}}
	value, err := EncodeReceipt(receipt)
	if err != nil {
		s.metric("unavailable", "other")
		return
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
	var f *Failure
	if !errors.As(err, &f) {
		s.fail(w, 503, "unavailable", "other")
		return
	}
	switch f.Code {
	case "too_large", "gone":
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
	if status == 401 || status == 403 || status == 503 {
		s.metric(code, reason)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorResponse{Version: 1, Code: code})
}
func (s *Server) metric(code, reason string) {
	if s.config.Metric != nil {
		s.config.Metric(code, reason)
	}
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
