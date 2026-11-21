package api

import (
	"context"
	"github.com/oberthci/oberth/internal/apicontract"
	"io"
	"mime"
	"net/http"
	"sync"
	"time"
)

func (s *Server) nativeStream(w http.ResponseWriter, r *http.Request, c apicontract.Caller) {
	ctx, cancelObservation := s.observationContext(r.Context())
	defer cancelObservation()
	r = r.WithContext(ctx)

	if s.config.Streams == nil || metricsOnly(c) {
		s.writeError(w, r, refusal("forbidden"), 403)
		return
	}
	var args apicontract.ContainerArgs
	raw, err := queryObject(r, schemaForType[apicontract.ContainerArgs]())
	if err == nil {
		err = decode(raw, &args, apicontract.RequestBytes)
	}
	if err == nil {
		err = validateInput(args)
	}
	if err != nil {
		s.writeError(w, r, err, 400)
		return
	}
	release, err := s.reservations.stream("native", c.Actor.Identity, nativePerIdentity)
	if err != nil {
		w.Header().Set("Retry-After", "1")
		s.writeError(w, r, err, 429)
		return
	}
	defer release()
	source, err := s.config.Streams.OpenLog(r.Context(), c, args)
	if err != nil {
		s.writeError(w, r, err, 0)
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	joined := make(chan struct{})
	closeSource := sync.OnceFunc(func() { _ = source.Close() })
	go func() { defer close(joined); s.watchLog(ctx, logWatch{r, c, args.ID, closeSource, cancel}) }()
	defer func() { cancel(); closeSource(); <-joined }()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_ = s.copyStream(w, source)
}

type logWatch struct {
	request     *http.Request
	caller      apicontract.Caller
	id          string
	closeSource func()
	cancel      context.CancelFunc
}

func (s *Server) watchLog(ctx context.Context, state logWatch) {
	r, c, id, cancel := state.request, state.caller, state.id, state.cancel
	defer cancel()
	defer state.closeSource()
	ticks, stop := s.config.Runtime.Ticker(time.Second)
	defer stop()
	lastAuth := s.config.Runtime.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.drain:
			return
		case <-ticks:
			if err := s.checkLog(ctx, c, id); err != nil {
				return
			}
			if s.config.Runtime.Now().Sub(lastAuth) >= 30*time.Second {
				if !s.stillAuthorized(r, c) {
					return
				}
				lastAuth = s.config.Runtime.Now()
			}
		}
	}
}
func (s *Server) checkLog(ctx context.Context, c apicontract.Caller, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return s.config.Streams.CheckLog(ctx, c, id)
}
func (s *Server) copyStream(w http.ResponseWriter, source io.Reader) error {
	ctrl := http.NewResponseController(w)
	if err := ctrl.SetWriteDeadline(time.Time{}); err != nil {
		return err
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := source.Read(buf)
		if n > 0 {
			if deadlineErr := ctrl.SetWriteDeadline(s.config.Runtime.Now().Add(logWriteTimeout)); deadlineErr != nil {
				return deadlineErr
			}
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
			if flushErr := ctrl.Flush(); flushErr != nil {
				return flushErr
			}
			if deadlineErr := ctrl.SetWriteDeadline(time.Time{}); deadlineErr != nil {
				return deadlineErr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
func (s *Server) backup(w http.ResponseWriter, r *http.Request, c apicontract.Caller) {
	ctx, cancelObservation := s.observationContext(r.Context())
	defer cancelObservation()
	r = r.WithContext(ctx)

	if !apicontract.Admin.Allows(c.Actor) || s.config.Streams == nil {
		s.writeError(w, r, refusal("forbidden"), 403)
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" || r.URL.RawQuery != "" {
		s.writeError(w, r, refusal("media"), 400)
		return
	}
	raw, err := readBody(w, r, apicontract.AdminBytes)
	var args apicontract.Empty
	if err == nil {
		err = decode(raw, &args, apicontract.AdminBytes)
	}
	if err != nil {
		s.writeError(w, r, err, 400)
		return
	}
	source, err := s.config.Streams.Backup(ctx, c, args)
	if err != nil {
		s.writeError(w, r, err, 0)
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	joined := make(chan struct{})
	go func() { defer close(joined); <-ctx.Done(); _ = source.Close() }()
	defer func() { cancel(); <-joined }()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="oberth-database-backup.tar"`)
	_ = s.copyStream(w, source)
}
