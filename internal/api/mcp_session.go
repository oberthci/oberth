package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// MCP sessions and server push (#1514). The POST endpoint stays stateless:
// every request authenticates and carries its own protocol version. A
// session only binds an events_subscribe filter and resources/subscribe set
// to the bearer credential that created it, so the streamable-HTTP GET
// stream can push agent events to it:
//
//   - notifications/claude/channel, the session-push notification Claude
//     Code accepts from a server declaring the experimental claude/channel
//     capability (launch with --channels server:<name>);
//   - notifications/resources/updated for each subscribed run://<id>.
//
// Delivery reads the durable event log after a cursor, so it is at-least-once
// with SSE event IDs; a reconnect resumes from Last-Event-ID. Bounds: open
// streams per credential, sessions per credential, a heartbeat, a bounded
// backlog replayed on connect, and per-frame write deadlines that drop a
// slow reader instead of buffering for it.

const (
	mcpSessionHeader          = "Mcp-Session-Id"
	defaultMCPStreamsPerToken = 4
	maxMCPStreamsPerToken     = 64
	defaultMCPEventBacklog    = 500
	maxMCPEventBacklog        = 10000
	mcpSessionsPerToken       = 16
	mcpSessionIdle            = time.Hour
	mcpResourcesPerSession    = 64
	mcpEventBatch             = 100
	mcpEventStreamReauth      = 30 * time.Second
	mcpChannelContentBytes    = 8 << 10
)

var mcpSessionPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

type mcpSession struct {
	id           string
	owner        string
	subscription EventSubscription
	resources    map[string]bool
	cursor       int64
	streams      int
	lastSeen     time.Time
}

type mcpSessions struct {
	mu      sync.Mutex
	byID    map[string]*mcpSession
	streams map[string]int
	// maxStreams bounds open GET streams per credential; maxBacklog bounds
	// the events replayed to one stream on connect.
	maxStreams int
	maxBacklog int
	now        func() time.Time
}

func newMCPSessions() *mcpSessions {
	return &mcpSessions{byID: map[string]*mcpSession{}, streams: map[string]int{},
		maxStreams: defaultMCPStreamsPerToken, maxBacklog: defaultMCPEventBacklog, now: time.Now}
}

// WithAgentEventStreams bounds the MCP push streams: open streams per bearer
// credential (1..64, default 4) and events replayed on connect (1..10000,
// default 500). Zero keeps a default.
func WithAgentEventStreams(maxStreamsPerToken, maxBacklog int) func(*Server) {
	return func(server *Server) {
		if maxStreamsPerToken > 0 {
			server.sessions.maxStreams = min(maxStreamsPerToken, maxMCPStreamsPerToken)
		}
		if maxBacklog > 0 {
			server.sessions.maxBacklog = min(maxBacklog, maxMCPEventBacklog)
		}
	}
}

// WithMCPHeartbeat sets the keepalive interval of MCP streams (default 20s).
func WithMCPHeartbeat(interval time.Duration) func(*Server) {
	return func(server *Server) {
		if interval > 0 {
			server.mcpHeartbeatInterval = interval
		}
	}
}

func sessionOwner(actor Actor) string {
	if actor.TokenCredentialID != "" {
		return "credential:" + actor.TokenCredentialID
	}
	return "identity:" + actor.Identity + ":" + actor.Fingerprint
}

func newMCPSessionID() string {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

// create opens a session for owner, evicting idle sessions and, beyond the
// per-credential bound, the owner's oldest session without a stream.
func (sessions *mcpSessions) create(id, owner string) *mcpSession {
	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	now := sessions.now()
	owned := []*mcpSession{}
	for key, session := range sessions.byID {
		if session.streams == 0 && now.Sub(session.lastSeen) > mcpSessionIdle {
			delete(sessions.byID, key)
			continue
		}
		if session.owner == owner {
			owned = append(owned, session)
		}
	}
	if len(owned) >= mcpSessionsPerToken {
		slices.SortFunc(owned, func(a, b *mcpSession) int { return a.lastSeen.Compare(b.lastSeen) })
		for _, session := range owned {
			if session.streams == 0 {
				delete(sessions.byID, session.id)
				break
			}
		}
	}
	if id == "" {
		id = newMCPSessionID()
	}
	session := &mcpSession{id: id, owner: owner, resources: map[string]bool{}, lastSeen: now}
	sessions.byID[id] = session
	return session
}

// lookup returns the session named by the request header. A missing header
// is (nil, true). A session of another credential is (nil, false): the
// caller answers 404, never revealing it. An unknown well-formed ID (the
// server restarted) is adopted for this owner when adopt is set.
func (sessions *mcpSessions) lookup(request *http.Request, owner string, adopt bool) (*mcpSession, bool) {
	values := request.Header.Values(mcpSessionHeader)
	if len(values) == 0 {
		return nil, true
	}
	id := strings.TrimSpace(values[0])
	if len(values) != 1 || !mcpSessionPattern.MatchString(id) {
		return nil, false
	}
	sessions.mu.Lock()
	session := sessions.byID[id]
	if session != nil {
		if session.owner != owner {
			sessions.mu.Unlock()
			return nil, false
		}
		session.lastSeen = sessions.now()
		sessions.mu.Unlock()
		return session, true
	}
	sessions.mu.Unlock()
	if !adopt {
		return nil, true
	}
	return sessions.create(id, owner), true
}

func (sessions *mcpSessions) acquireStream(session *mcpSession) bool {
	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	if sessions.streams[session.owner] >= sessions.maxStreams {
		return false
	}
	sessions.streams[session.owner]++
	session.streams++
	return true
}

func (sessions *mcpSessions) releaseStream(session *mcpSession) {
	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	sessions.streams[session.owner]--
	if sessions.streams[session.owner] <= 0 {
		delete(sessions.streams, session.owner)
	}
	session.streams--
	session.lastSeen = sessions.now()
}

func (sessions *mcpSessions) remove(session *mcpSession) {
	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	delete(sessions.byID, session.id)
}

func (sessions *mcpSessions) state(session *mcpSession) (EventSubscription, map[string]bool, int64) {
	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	resources := make(map[string]bool, len(session.resources))
	for id := range session.resources {
		resources[id] = true
	}
	return session.subscription, resources, session.cursor
}

func (sessions *mcpSessions) setSubscription(session *mcpSession, subscription EventSubscription) {
	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	session.subscription = subscription
}

func (sessions *mcpSessions) setCursor(session *mcpSession, cursor int64) {
	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	session.cursor = max(session.cursor, cursor)
}

func (sessions *mcpSessions) subscribeResource(session *mcpSession, runID string, subscribe bool) error {
	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	if !subscribe {
		delete(session.resources, runID)
		return nil
	}
	if !session.resources[runID] && len(session.resources) >= mcpResourcesPerSession {
		return fmt.Errorf("at most %d run resources per session", mcpResourcesPerSession)
	}
	session.resources[runID] = true
	return nil
}

// runResourceID parses run://<id>.
func runResourceID(uri string) (string, bool) {
	id, ok := strings.CutPrefix(uri, "run://")
	return id, ok && id != "" && len(id) <= 64 && !strings.ContainsAny(id, "/?# ")
}

// handleMCPResources serves resources/* for run://<id>. A run outside the
// caller's scope answers exactly like a missing run.
func (server *Server) handleMCPResources(request *http.Request, session *mcpSession, method string, raw json.RawMessage, response *rpcResponse) {
	switch method {
	case "resources/list":
		response.Result = map[string]any{"resources": []any{}}
		return
	case "resources/templates/list":
		response.Result = map[string]any{"resourceTemplates": []map[string]string{{
			"uriTemplate": "run://{id}", "name": "run", "mimeType": "application/json",
			"description": "One run by full ID or unique 8+ character prefix; subscribe for notifications/resources/updated on every agent event about it",
		}}}
		return
	}
	var params struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		response.Error = &rpcError{Code: -32602, Message: "resources params require a uri"}
		return
	}
	id, ok := runResourceID(params.URI)
	if !ok {
		response.Error = &rpcError{Code: -32602, Message: "only run://<id> resources exist"}
		return
	}
	value, err := server.views.Run(request.Context(), actorFrom(request.Context()), id)
	if err != nil {
		_, message := server.classifyError(err)
		response.Error = &rpcError{Code: -32002, Message: "resource unavailable: " + message}
		return
	}
	canonical := id
	if detail, ok := value.(RunDetailResponse); ok && detail.Run.ID != "" {
		canonical = detail.Run.ID
	}
	switch method {
	case "resources/read":
		encoded, err := marshalCompatible(value)
		if err != nil {
			response.Error = &rpcError{Code: -32603, Message: "internal error"}
			return
		}
		response.Result = map[string]any{"contents": []map[string]string{{"uri": "run://" + canonical, "mimeType": "application/json", "text": string(encoded)}}}
	case "resources/subscribe", "resources/unsubscribe":
		if session == nil {
			response.Error = &rpcError{Code: -32600, Message: "resources/subscribe needs an MCP session: send the Mcp-Session-Id from initialize"}
			return
		}
		if err := server.sessions.subscribeResource(session, canonical, method == "resources/subscribe"); err != nil {
			response.Error = &rpcError{Code: -32602, Message: err.Error()}
			return
		}
		response.Result = map[string]any{}
	}
}

// handleMCPEventStream is the streamable-HTTP GET stream of one session.
func (server *Server) handleMCPEventStream(writer http.ResponseWriter, request *http.Request) {
	feed, ok := server.tools.(EventFeed)
	if !ok || feed.AgentEventWake() == nil {
		writer.Header().Set("Allow", "POST")
		http.Error(writer, "server push is unavailable", http.StatusMethodNotAllowed)
		return
	}
	if !acceptsMCPStream(request.Header) {
		http.Error(writer, "the MCP GET stream requires Accept: text/event-stream", http.StatusNotAcceptable)
		return
	}
	if !supportsMCPFlush(writer) {
		http.Error(writer, "streaming unavailable", http.StatusInternalServerError)
		return
	}
	actor := actorFrom(request.Context())
	owner := sessionOwner(actor)
	session, ok := server.sessions.lookup(request, owner, true)
	if !ok {
		http.Error(writer, "unknown MCP session", http.StatusNotFound)
		return
	}
	if session == nil {
		session = server.sessions.create("", owner)
	}
	if !server.sessions.acquireStream(session) {
		writer.Header().Set("Retry-After", "30")
		http.Error(writer, fmt.Sprintf("this credential already has %d open MCP event streams", server.sessions.maxStreams), http.StatusTooManyRequests)
		return
	}
	defer server.sessions.releaseStream(session)
	ctx := request.Context()
	probe, err := feed.AgentEventsFor(ctx, actor, EventSubscription{}, math.MaxInt64-1, 1)
	if err != nil {
		_, message := server.classifyError(err)
		http.Error(writer, message, http.StatusServiceUnavailable)
		return
	}
	_, _, cursor := server.sessions.state(session)
	if raw := strings.TrimSpace(request.Header.Get("Last-Event-ID")); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed >= 0 {
			cursor = parsed
		}
	}
	if cursor == 0 || cursor > probe.LatestID {
		cursor = probe.LatestID
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Accel-Buffering", "no")
	writer.Header().Set(mcpSessionHeader, session.id)
	interval := server.mcpHeartbeatInterval
	if interval <= 0 {
		interval = 20 * time.Second
	}
	writeTimeout := server.mcpStreamWriteTimeout
	if writeTimeout <= 0 {
		writeTimeout = 5 * time.Second
	}
	controller := http.NewResponseController(writer)
	defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	write := func(frame string) error {
		return writeMCPFrame(writer, frame, time.Now().Add(2*interval+writeTimeout), writeTimeout)
	}
	if err := write(": oberth agent events\n\n"); err != nil {
		return
	}
	if skipped := probe.LatestID - cursor; skipped > int64(server.sessions.maxBacklog) {
		resume := cursor
		cursor = probe.LatestID - int64(server.sessions.maxBacklog)
		notice := fmt.Sprintf("%d older agent events were not replayed on this stream (agentEvents.maxBacklog); read them with events_since after_id %d", cursor-resume, resume)
		if write(channelFrame(0, notice, map[string]string{"kind": "events.backlog_truncated", "after_id": strconv.FormatInt(resume, 10)})) != nil {
			return
		}
	}
	heartbeat := time.NewTicker(interval)
	defer heartbeat.Stop()
	lastAuth := time.Now()
	for {
		wake := feed.AgentEventWake()
		// Bounded per wake: a backlog drains in batches across iterations.
		for range server.sessions.maxBacklog/mcpEventBatch + 1 {
			subscription, resources, _ := server.sessions.state(session)
			page, err := feed.AgentEventsFor(ctx, actor, EventSubscription{}, cursor, mcpEventBatch)
			if err != nil {
				return
			}
			for _, event := range page.Events {
				if subscription.Matches(event, actor.Identity) {
					if write(eventChannelFrame(event)) != nil {
						return
					}
				}
				if event.RunID != "" && resources[event.RunID] {
					if write(resourceUpdatedFrame(event)) != nil {
						return
					}
				}
			}
			cursor = max(cursor, page.NextAfterID)
			server.sessions.setCursor(session, cursor)
			if !page.HasMore {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-server.drainContext.Done():
			return
		case <-wake:
		case <-heartbeat.C:
			if write(": keepalive\n\n") != nil {
				return
			}
		}
		if time.Since(lastAuth) >= mcpEventStreamReauth {
			if !server.reauthorizeStream(request, actor) {
				_ = write(channelFrame(0, "agent event stream authorization changed or expired; reconnect with a current bearer token", map[string]string{"kind": "events.auth_error"}))
				return
			}
			lastAuth = time.Now()
		}
	}
}

// reauthorizeStream re-checks the bearer token so a revoked or narrowed
// credential cannot keep receiving events on an old stream.
func (server *Server) reauthorizeStream(request *http.Request, original Actor) bool {
	token, ok := bearerToken(request.Header.Values("Authorization"))
	if !ok {
		return false
	}
	authCtx, cancel := context.WithTimeout(request.Context(), 5*time.Second)
	defer cancel()
	fresh, err := server.auth.Authenticate(authCtx, token)
	return err == nil && strings.TrimSpace(fresh.Identity) != "" && fresh.Identity == original.Identity && fresh.Fingerprint == original.Fingerprint &&
		fresh.Admin == original.Admin && slices.Equal(fresh.Scope, original.Scope)
}

func sseFrame(id int64, message any) string {
	encoded, err := json.Marshal(message)
	if err != nil {
		return ""
	}
	if id > 0 {
		return "id: " + strconv.FormatInt(id, 10) + "\nevent: message\ndata: " + string(encoded) + "\n\n"
	}
	return "event: message\ndata: " + string(encoded) + "\n\n"
}

func channelFrame(id int64, content string, meta map[string]string) string {
	return sseFrame(id, map[string]any{"jsonrpc": "2.0", "method": "notifications/claude/channel", "params": map[string]any{"content": content, "meta": meta}})
}

// eventChannelFrame renders one agent event as a channel notification: the
// one-line summary and the full event as JSON in content, and identifier
// keyed string attributes in meta.
func eventChannelFrame(event model.AgentEvent) string {
	encoded, _ := json.Marshal(event)
	content := event.Summary
	if len(encoded) <= mcpChannelContentBytes {
		content += "\n" + string(encoded)
	}
	meta := map[string]string{"kind": event.Kind, "event_id": strconv.FormatInt(event.ID, 10), "at": event.At.UTC().Format(time.RFC3339)}
	for key, value := range map[string]string{"repo": event.Repo, "run_id": event.RunID, "promotion_id": event.PromotionID, "ref": event.Ref, "sha": event.SHA} {
		if value != "" {
			meta[key] = value
		}
	}
	for _, key := range []string{"status", "failed_step", "step", "pod", "held_since", "abandon_at", "abandoned_at", "abandon_reason", "failed_exit_code", "holds_in_run", "lock", "waited_s", "attempt", "final"} {
		switch value := event.Data[key].(type) {
		case string:
			meta[key] = value
		case float64:
			meta[key] = strconv.FormatFloat(value, 'f', -1, 64)
		case int:
			meta[key] = strconv.Itoa(value)
		case int64:
			meta[key] = strconv.FormatInt(value, 10)
		case bool:
			meta[key] = strconv.FormatBool(value)
		}
	}
	if hint, ok := event.Data["retry_hint"].(map[string]any); ok {
		if class, ok := hint["class"].(string); ok {
			meta["retry_class"] = class
		}
	}
	return channelFrame(event.ID, content, meta)
}

func resourceUpdatedFrame(event model.AgentEvent) string {
	return sseFrame(event.ID, map[string]any{"jsonrpc": "2.0", "method": "notifications/resources/updated", "params": map[string]any{"uri": "run://" + event.RunID}})
}
