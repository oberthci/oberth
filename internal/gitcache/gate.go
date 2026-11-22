package gitcache

import (
	"context"
	"errors"
	"github.com/oberthci/oberth/internal/publicerr"
	"net"
	"sync"
)

type gateSession struct {
	store, owner string
	passed       func()
	refused      func()
	progress     *receiveProgress
}
type receiveGate struct {
	address  string
	listener net.Listener
	cancel   context.CancelFunc
	done     chan error
	once     sync.Once
	err      error
	passed   bool
	refused  bool
}

func (c *Cache) startGate(ctx context.Context, session gateSession) (*receiveGate, error) {
	ctx, cancel := context.WithCancel(ctx)
	address := "@oberth-" + session.owner
	var config net.ListenConfig
	listener, err := config.Listen(ctx, "unix", address)
	if err != nil {
		cancel()
		return nil, &gateError{code: "unavailable", cause: err}
	}
	g := &receiveGate{address: address, listener: listener, cancel: cancel, done: make(chan error, 1)}
	session.passed = func() { g.passed = true }
	session.refused = func() { g.refused = true }
	go func() { g.done <- c.serveGate(ctx, listener, session) }()
	return g, nil
}
func (g *receiveGate) close() error {
	g.once.Do(func() { g.cancel(); _ = g.listener.Close(); g.err = <-g.done })
	return g.err
}
func (c *Cache) serveGate(ctx context.Context, listener net.Listener, session gateSession) error {
	held := 0
	var result error
	defer func() {
		if held > 0 {
			c.unlock(session.store)
		}
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return result
			}
			return err
		}
		err = c.serveGateCall(ctx, conn, session, &held)
		result = errors.Join(result, err)
	}
}
func (c *Cache) serveGateCall(ctx context.Context, conn net.Conn, session gateSession, held *int) error {
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var request gateRequest
	err := readGateFrame(conn, &request)
	if err == nil {
		err = c.gateOperation(ctx, session, request, held)
		if err != nil && session.refused != nil {
			session.refused()
		}
	}
	reply := gateReply{Code: "accepted"}
	if err != nil {
		reply.Code = "unavailable"
		var refusal *gateError
		if errors.As(err, &refusal) {
			reply.Code = refusal.code
			reply.Detail = refusal.detail
		}
	}
	return errors.Join(err, writeGateFrame(conn, reply))
}
func (c *Cache) gateOperation(ctx context.Context, session gateSession, request gateRequest, held *int) error {
	session.progress.process()
	switch request.Phase {
	case "refused":
		return &gateError{code: "refused", detail: request.Refusal}
	case "":
		return c.admitReceive(ctx, session, request)
	case "preparing":
		// Git has not locked refs; publication locking begins at prepared.
		return nil
	case "prepared":
		return c.prepareTransaction(ctx, session, request, held)
	case "committed", "aborted":
		if *held > 0 {
			*held--
			if *held == 0 {
				c.unlock(session.store)
			}
		}
		return nil
	default:
		return refuse("receive transaction")
	}
}

func (c *Cache) prepareTransaction(ctx context.Context, session gateSession, request gateRequest, held *int) error {
	if *held > 0 {
		*held++
		return c.markReceivePrepared(session, request.Updates)
	}
	if c.receivePrepared != nil {
		c.receivePrepared()
	}
	err := c.lock(ctx, session.store)
	if err != nil {
		return err
	}
	*held = 1
	if c.receivePublicationBusy(session.store) {
		c.unlock(session.store)
		*held = 0
		return &gateError{code: "unavailable", detail: reasonPublishing + ". " + remedyPublishing}
	}
	return c.markReceivePrepared(session, request.Updates)
}

func (c *Cache) markReceivePrepared(session gateSession, updates []RefUpdate) error {
	record, err := readReservation(session.store)
	if err != nil {
		return err
	}
	if record.State != "ready" {
		return refuse("receive identity")
	}
	if files := c.receiveCleanupFor(session.store); files != nil {
		files.captureReferences(session.store, updates)
	}
	if !recordPreparation(&record, updates) {
		return nil
	}
	return writeReservation(session.store, record)
}

// Repeated phase notifications do not rewrite facts already recorded.
func recordPreparation(record *Reservation, updates []RefUpdate) bool {
	changed := record.Prepared == nil
	if changed {
		record.Prepared = make(map[string]struct{})
	}
	for _, update := range updates {
		for _, admitted := range record.Event.Updates {
			if update.Ref == admitted.Ref {
				_, exists := record.Prepared[update.Ref]
				if !exists {
					record.Prepared[update.Ref], changed = struct{}{}, true
				}
			}
		}
	}
	return changed
}

func (c *Cache) admitReceive(ctx context.Context, session gateSession, request gateRequest) error {
	record, err := readReservation(session.store)
	if err != nil {
		return err
	}
	if "receive-"+record.Event.ID != session.owner || record.State != "reserved" {
		return refuse("receive identity")
	}
	source, err := c.receiveSource(ctx, session.store, record, request.Quarantine)
	if err != nil {
		return err
	}
	return c.checkAdmission(ctx, session, record.Event, source)
}
func (
	c *Cache,
) checkAdmission(
	ctx context.Context,
	session gateSession,
	event ReceiveEvent,
	source *ReceiveSource,
) error {
	if c.config.PreReceive == nil {
		return &gateError{code: "unavailable"}
	}
	ctx, cancel := context.WithTimeout(ctx, c.config.AdmissionTimeout)
	defer cancel()
	source.ctx = ctx
	if session.passed != nil {
		session.passed()
	}
	done := make(chan error, 1)
	go func() { done <- c.config.PreReceive(ctx, event, source) }()
	answered, err := c.admissionAnswer(ctx, event.ID, source, done)
	switch {
	case source.delivered.Load():
		return deliveredReply(answered, err)
	case source.delivering.Load() && !answered:
		// A server stop ended the wait while the forge write was under way:
		// whether the tag is there is unknown, so the reply claims neither.
		return &gateError{code: "unavailable", cause: context.Cause(ctx)}
	}
	return c.admissionVerdict(ctx, source, answered, err)
}

// admissionVerdict is admissionReply, and logs an admission refused at its
// deadline although its callback had returned, so every deadline refusal is
// logged once with its repository (one whose callback was still running is
// logged by admissionAnswer).
func (c *Cache) admissionVerdict(ctx context.Context, source *ReceiveSource, answered bool, err error) error {
	reply := admissionReply(ctx, answered, err)
	var refusal *gateError
	if answered && c.config.Log != nil && errors.As(reply, &refusal) && refusal.deadline {
		c.config.Log(reasonAdmissionEnded, source.event.Repository+": "+source.event.ID)
	}
	return reply
}

// deliveredReply answers for a receive whose release tag is already on the
// forge. Its approval stands, also a late one: refusing would spend the
// version with no run. Anything else (an error, a server stop) says that the
// tag was delivered and its version is used, after the callback's own reason
// when it gave one; never that no ref moved.
func deliveredReply(answered bool, err error) error {
	if answered && err == nil {
		return nil
	}
	detail := reasonTagDelivered + ". " + remedyTagDelivered
	if answered {
		detail = err.Error() + "\n" + detail
	}
	code := "unavailable"
	if publicerr.Permanent(err) {
		code = "refused"
	}
	return &gateError{code: code, detail: detail, cause: err}
}

// admissionReply turns the callback's answer into the gate's. A refusal is
// returned as the callback gave it, also when it came as the admission
// context ended, if it names its reason; an approval counts only while the
// context is live; an admission that ran out of time says so.
func admissionReply(ctx context.Context, answered bool, err error) error {
	var public *publicerr.Error
	switch {
	case answered && err != nil && (ctx.Err() == nil || errors.As(err, &public)):
		code := "unavailable"
		if publicerr.Permanent(err) {
			code = "refused"
		}
		return &gateError{code: code, detail: err.Error(), cause: err}
	case ctx.Err() == nil:
		return nil
	case errors.Is(context.Cause(ctx), context.DeadlineExceeded):
		return &gateError{code: "unavailable", detail: reasonGateTimeout + ". " + remedyGateTimeout, cause: context.Cause(ctx), deadline: true}
	default:
		return &gateError{code: "unavailable", cause: context.Cause(ctx)}
	}
}

// Composition supplies the admission duration. Enforce its cancellation even when a
// broken callback ignores it; a late result cannot approve or retain the source,
// unless the callback began delivering a release tag: it is then awaited, since
// a refusal could spend the tag's version with no run (deliveredReply answers
// a delivered one). Otherwise an answer already in done when the context
// ends is taken without waiting; answered is false only when the callback had
// not returned.
func (c *Cache) admissionAnswer(ctx context.Context, id string, source *ReceiveSource, done <-chan error) (bool, error) {
	select {
	case err := <-done:
		return true, err
	case <-ctx.Done():
		if source.delivering.Load() {
			// A forge write under way, or done, is waited for: its outcome decides
			// the answer. The cache's own end still ends the wait, so a server
			// stop is never held.
			select {
			case err := <-done:
				return true, err
			case <-c.ctx.Done():
				return false, context.Cause(ctx)
			}
		}
		select {
		case err := <-done:
			return true, err
		default:
			if c.config.AdmissionMetric != nil {
				c.config.AdmissionMetric(reasonAdmissionStuck)
			}
			if c.config.Log != nil {
				c.config.Log(reasonAdmissionStuck, source.event.Repository+": "+id)
			}
		}
		return false, context.Cause(ctx)
	}
}
