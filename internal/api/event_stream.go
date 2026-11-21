package api

import (
	"context"
	"encoding/json"
	"github.com/oberthci/oberth/internal/apicontract"
	"net/http"
	"strconv"
)

func (s *Server) eventStream(w http.ResponseWriter, r *http.Request, c apicontract.Caller) {
	ctx, cancelObservation := s.observationContext(r.Context())
	defer cancelObservation()
	r = r.WithContext(ctx)

	if !acceptsSSE(r.Header) || s.config.Registry.events == nil {
		s.writeError(w, r, refusal("eventMedia"), 405)
		return
	}
	id, state, err := s.sessions.lookup(r, c, s.config.Runtime.Now())
	if err != nil {
		s.writeError(w, r, err, 404)
		return
	}
	cursor := state.cursor
	if values := r.Header.Values("Last-Event-ID"); len(values) > 0 {
		if len(values) != 1 {
			s.writeError(w, r, refusal("input"), 400)
			return
		}
		n, err := strconv.ParseInt(values[0], 10, 64)
		if err != nil || n < 0 {
			s.writeError(w, r, refusal("input"), 400)
			return
		}
		cursor = &n
	}
	release, err := s.reservations.stream("events", c.Actor.Identity, s.config.EventStreams)
	if err != nil {
		w.Header().Set("Retry-After", "1")
		s.writeError(w, r, err, 429)
		return
	}
	defer release()
	args := apicontract.EventsWaitArgs{AfterID: cursor, Limit: min(100, s.config.ReplayBacklog), TimeoutS: 20}
	call, err := s.config.Registry.events.Prepare(ctx, c, args)
	if err != nil {
		s.writeError(w, r, err, 0)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	if err := s.frame(w, ": keepalive\n\n"); err != nil {
		return
	}
	s.deliverEvents(ctx, w, eventDelivery{r, c, id, args, call})
}

type eventDelivery struct {
	request *http.Request
	caller  apicontract.Caller
	id      string
	args    apicontract.EventsWaitArgs
	call    apicontract.Call[apicontract.EventResult]
}

func (s *Server) deliverEvents(ctx context.Context, w http.ResponseWriter, d eventDelivery) {
	r, c, id, args, call := d.request.WithContext(ctx), d.caller, d.id, d.args, d.call
	backlog := 0
	for ctx.Err() == nil {
		value, err := s.withHeartbeats(w, r, c, observation{call: func(ctx context.Context) (any, error) { return call(ctx) }})
		if s.draining.Load() {
			_ = s.frame(w, notification("notifications/cancelled", drainRPC()))
			return
		}
		if err != nil {
			return
		}
		page, ok := validatedEventPage(value, args.Limit)
		if !ok {
			return
		}
		backlog += len(page.Events)
		capped := page.HasMore && backlog >= s.config.ReplayBacklog
		if capped {
			page.Gap = true
		}
		state, authorized := s.deliveryAuthorized(r, c)
		if !authorized {
			return
		}
		if err := s.writeEventPage(w, state, page); err != nil {
			return
		}
		s.sessions.update(id, func(v *session) { cursor := page.NextAfterID; v.cursor = &cursor; v.last = s.config.Runtime.Now() })
		if capped {
			return
		}
		if !page.HasMore {
			backlog = 0
		}
		args.AfterID = &page.NextAfterID
		args.Limit = min(100, s.config.ReplayBacklog-backlog)
		call, err = s.config.Registry.events.Prepare(ctx, c, args)
		if err != nil {
			return
		}
	}
}
func (s *Server) resource(w http.ResponseWriter, r *http.Request, c apicontract.Caller, x rpcExchange) {
	req, resp := x.req, x.resp
	id, _, err := s.sessions.lookup(r, c, s.config.Runtime.Now())
	if err != nil {
		resp.Error = &rpcError{-32000, refusal("session").Reason(), nil}
		s.writeRPC(w, r, resp)
		return
	}
	var params struct {
		URI string `json:"uri,omitempty"`
	}
	if err := decode(req.Params, &params, apicontract.RequestBytes); err != nil {
		resp.Error = &rpcError{-32602, refusal("input").Reason(), nil}
		s.writeRPC(w, r, resp)
		return
	}
	if req.Method == "resources/list" {
		resp.Result = map[string]any{"resources": []any{}}
		s.writeRPC(w, r, resp)
		return
	}
	run, ok := resourceID(params.URI)
	if !ok {
		resp.Error = &rpcError{-32602, refusal("input").Reason(), nil}
		s.writeRPC(w, r, resp)
		return
	}
	arguments, _ := json.Marshal(apicontract.RunGetArgs{ID: run})
	value, err := s.config.Registry.Invoke(r.Context(), c, "run_get", arguments)
	if err != nil {
		e := s.publicError(err)
		resp.Error = &rpcError{-32000, e.Reason(), e}
		s.writeRPC(w, r, resp)
		return
	}
	if req.Method == "resources/read" {
		encoded, _ := json.Marshal(value)
		resp.Result = map[string]any{"contents": []any{map[string]any{"uri": params.URI, "mimeType": "application/json", "text": string(encoded)}}}
	} else {
		if err := s.sessions.resource(id, run, req.Method == "resources/subscribe"); err != nil {
			problem := s.publicError(err)
			resp.Error = &rpcError{-32000, problem.Reason(), problem}
		} else {
			resp.Result = apicontract.Empty{}
		}

	}
	s.writeRPC(w, r, resp)
}
func resourceID(uri string) (string, bool) {
	const prefix = "run://"
	if len(uri) <= len(prefix) || uri[:len(prefix)] != prefix {
		return "", false
	}
	run := uri[len(prefix):]
	for _, r := range run {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return "", false
		}
	}
	return run, true
}

func (s *Server) deliveryAuthorized(r *http.Request, c apicontract.Caller) (*session, bool) {
	_, state, err := s.sessions.lookup(r, c, s.config.Runtime.Now())
	if err != nil {
		return nil, false
	}
	return state, s.stillAuthorized(r, c)
}

func validEventPage(page apicontract.EventResult, limit int) bool {
	if len(page.Events) > limit {
		return false
	}
	_, err := encodeBounded(page, apicontract.ResponseBytes)
	return err == nil
}

func validatedEventPage(value any, limit int) (apicontract.EventResult, bool) {
	page, ok := value.(apicontract.EventResult)
	if !ok {
		return apicontract.EventResult{}, false
	}
	return page, validEventPage(page, limit)
}

func (s *Server) writeEventPage(w http.ResponseWriter, state *session, page apicontract.EventResult) error {
	frame := "id: " + strconv.FormatInt(page.NextAfterID, 10) + "\n" + notification("notifications/events", page)
	if err := s.frame(w, frame); err != nil {
		return err
	}
	for _, event := range page.Events {
		if state.resources[event.RunID] {
			if err := s.frame(w, notification("notifications/resources/updated", struct {
				URI string `json:"uri"`
			}{"run://" + event.RunID})); err != nil {
				return err
			}
		}
	}
	return nil
}
