package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// eventBackend is an EventFeed whose scope is by identity: other@host sees
// only repository b, agent@host sees everything.
type eventBackend struct {
	*fakeBackend
	mu     sync.Mutex
	events []model.AgentEvent
	wake   chan struct{}
}

func newEventBackend() *eventBackend {
	return &eventBackend{fakeBackend: &fakeBackend{}, wake: make(chan struct{})}
}

func (backend *eventBackend) Authenticate(_ context.Context, token string) (Actor, error) {
	switch token {
	case "valid-token":
		return Actor{Identity: "agent@host", Fingerprint: "SHA256:key", TokenCredentialID: "cred-a"}, nil
	case "other-token":
		return Actor{Identity: "other@host", Fingerprint: "SHA256:other", TokenCredentialID: "cred-b"}, nil
	}
	return Actor{}, ErrInvalidToken
}

func (backend *eventBackend) CallTool(ctx context.Context, actor Actor, name string, arguments json.RawMessage) (any, error) {
	if name == "events_subscribe" {
		var subscription EventSubscription
		_ = json.Unmarshal(arguments, &subscription)
		return EventsSubscribeResponse{Subscription: subscription}, nil
	}
	return backend.fakeBackend.CallTool(ctx, actor, name, arguments)
}

func (backend *eventBackend) AgentEventWake() <-chan struct{} {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return backend.wake
}

func (backend *eventBackend) publish(event model.AgentEvent) {
	backend.mu.Lock()
	event.ID = int64(len(backend.events) + 1)
	backend.events = append(backend.events, event)
	close(backend.wake)
	backend.wake = make(chan struct{})
	backend.mu.Unlock()
}

func (backend *eventBackend) AgentEventsFor(_ context.Context, actor Actor, subscription EventSubscription, after int64, limit int) (EventPage, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	page := EventPage{Events: []model.AgentEvent{}, NextAfterID: after, LatestID: int64(len(backend.events))}
	for _, event := range backend.events {
		if event.ID <= after {
			continue
		}
		if len(page.Events) == limit {
			page.HasMore = true
			break
		}
		page.NextAfterID = event.ID
		if actor.Identity == "other@host" && event.Repo != "b" {
			continue
		}
		if subscription.Matches(event, actor.Identity) {
			page.Events = append(page.Events, event)
		}
	}
	return page, nil
}

func eventServer(t *testing.T, options ...func(*Server)) (string, *eventBackend, *Server) {
	t.Helper()
	backend := newEventBackend()
	options = append([]func(*Server){WithMCPHeartbeat(20 * time.Millisecond)}, options...)
	server, err := New(backend, backend, backend, "test", options...)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return httpServer.URL, backend, server
}

func postMCP(t *testing.T, url, token, session, body string) (*http.Response, map[string]any) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, url+"/mcp", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	if session != "" {
		request.Header.Set(mcpSessionHeader, session)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var envelope map[string]any
	_ = json.NewDecoder(response.Body).Decode(&envelope)
	return response, envelope
}

func initializeSession(t *testing.T, url, token string) string {
	t.Helper()
	response, envelope := postMCP(t, url, token, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	session := response.Header.Get(mcpSessionHeader)
	result, _ := envelope["result"].(map[string]any)
	capabilities, _ := result["capabilities"].(map[string]any)
	experimental, _ := capabilities["experimental"].(map[string]any)
	resources, _ := capabilities["resources"].(map[string]any)
	if !mcpSessionPattern.MatchString(session) || experimental["claude/channel"] == nil || resources["subscribe"] != true {
		t.Fatalf("initialize = %v session %q", envelope, session)
	}
	return session
}

type sseReader struct {
	response *http.Response
	lines    chan string
}

func openStream(t *testing.T, url, token, session, lastEventID string) (*sseReader, int) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/mcp", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "text/event-stream")
	if session != "" {
		request.Header.Set(mcpSessionHeader, session)
	}
	if lastEventID != "" {
		request.Header.Set("Last-Event-ID", lastEventID)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		return nil, response.StatusCode
	}
	reader := &sseReader{response: response, lines: make(chan string, 256)}
	go func() {
		defer close(reader.lines)
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 64<<10), 1<<20)
		for scanner.Scan() {
			reader.lines <- scanner.Text()
		}
	}()
	t.Cleanup(func() { _ = response.Body.Close() })
	return reader, http.StatusOK
}

type sseMessage struct {
	id      string
	method  string
	params  map[string]any
	comment bool
}

// next returns the next frame, failing after a bound.
func (reader *sseReader) next(t *testing.T) sseMessage {
	t.Helper()
	var message sseMessage
	deadline := time.After(5 * time.Second)
	for {
		select {
		case line, ok := <-reader.lines:
			if !ok {
				t.Fatal("stream closed")
			}
			switch {
			case strings.HasPrefix(line, ":"):
				message.comment = true
			case strings.HasPrefix(line, "id: "):
				message.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				var rpc struct {
					Method string         `json:"method"`
					Params map[string]any `json:"params"`
				}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &rpc); err != nil {
					t.Fatalf("frame %q: %v", line, err)
				}
				message.method, message.params = rpc.Method, rpc.Params
			case line == "":
				if message.comment || message.method != "" {
					return message
				}
			}
		case <-deadline:
			t.Fatal("no frame within bound")
		}
	}
}

// nextNotification skips heartbeats.
func (reader *sseReader) nextNotification(t *testing.T) sseMessage {
	t.Helper()
	for {
		if message := reader.next(t); message.method != "" {
			return message
		}
	}
}

func TestMCPEventStreamPushesChannelNotifications(t *testing.T) {
	url, backend, _ := eventServer(t)
	session := initializeSession(t, url, "valid-token")
	stream, status := openStream(t, url, "valid-token", session, "")
	if status != http.StatusOK {
		t.Fatalf("GET = %d", status)
	}
	if first := stream.next(t); !first.comment {
		t.Fatalf("first frame = %+v", first)
	}
	backend.publish(model.AgentEvent{Kind: model.AgentEventRunHeld, Repo: "a", RunID: "run-1", Ref: "land/x", Summary: "run run-1 holds step test",
		Data: map[string]any{"step": "test", "pod": "pod-1", "held_since": "2026-11-09T19:00:00Z", "abandon_at": "2026-11-09T19:20:00Z", "holds_in_run": float64(2)}})
	message := stream.nextNotification(t)
	meta, _ := message.params["meta"].(map[string]any)
	content, _ := message.params["content"].(string)
	if message.method != "notifications/claude/channel" || message.id != "1" || meta["kind"] != "run.held" || meta["step"] != "test" ||
		meta["abandon_at"] != "2026-11-09T19:20:00Z" || meta["holds_in_run"] != "2" || meta["run_id"] != "run-1" || !strings.HasPrefix(content, "run run-1 holds step test\n{") {
		t.Fatalf("notification = %+v", message)
	}
	for key := range meta {
		if !regexpIdentifier(key) {
			t.Fatalf("meta key %q is not an identifier", key)
		}
	}
	// events_subscribe narrows this session; a held run keeps flowing.
	_, envelope := postMCP(t, url, "valid-token", session, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"events_subscribe","arguments":{"kinds":["run.held"]}}}`)
	result, _ := envelope["result"].(map[string]any)
	structured, _ := result["structuredContent"].(map[string]any)
	if structured["session"] != true || !strings.Contains(structured["push"].(string), session) {
		t.Fatalf("subscribe = %v", envelope)
	}
	backend.publish(model.AgentEvent{Kind: model.AgentEventRunFinished, Repo: "a", RunID: "run-2", Summary: "finished"})
	backend.publish(model.AgentEvent{Kind: model.AgentEventRunHeld, Repo: "a", RunID: "run-3", Summary: "held again"})
	if message := stream.nextNotification(t); message.id != "3" {
		t.Fatalf("filtered stream delivered %+v", message)
	}
}

func regexpIdentifier(key string) bool {
	for index, r := range key {
		letter := r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		if !letter && (index == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return key != ""
}

func TestMCPEventStreamResourcesAndScope(t *testing.T) {
	url, backend, _ := eventServer(t)
	session := initializeSession(t, url, "valid-token")
	_, envelope := postMCP(t, url, "valid-token", session, `{"jsonrpc":"2.0","id":2,"method":"resources/subscribe","params":{"uri":"run://run-7"}}`)
	if envelope["error"] != nil {
		t.Fatalf("subscribe = %v", envelope)
	}
	_, envelope = postMCP(t, url, "valid-token", session, `{"jsonrpc":"2.0","id":3,"method":"resources/read","params":{"uri":"run://run-7"}}`)
	if result, _ := envelope["result"].(map[string]any); result["contents"] == nil {
		t.Fatalf("read = %v", envelope)
	}
	if _, envelope := postMCP(t, url, "valid-token", session, `{"jsonrpc":"2.0","id":4,"method":"resources/read","params":{"uri":"file:///etc/passwd"}}`); envelope["error"] == nil {
		t.Fatalf("non-run resource = %v", envelope)
	}
	postMCP(t, url, "valid-token", session, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"events_subscribe","arguments":{"kinds":["release.published"]}}}`)
	stream, _ := openStream(t, url, "valid-token", session, "")
	stream.next(t)
	backend.publish(model.AgentEvent{Kind: model.AgentEventRunFinished, Repo: "a", RunID: "run-7", Summary: "finished"})
	if message := stream.nextNotification(t); message.method != "notifications/resources/updated" || message.params["uri"] != "run://run-7" {
		t.Fatalf("resource update = %+v", message)
	}

	// Another credential cannot use or even see this session.
	if response, _ := postMCP(t, url, "other-token", session, `{"jsonrpc":"2.0","id":6,"method":"ping"}`); response.StatusCode != http.StatusNotFound {
		t.Fatalf("foreign session POST = %d", response.StatusCode)
	}
	if _, status := openStream(t, url, "other-token", session, ""); status != http.StatusNotFound {
		t.Fatalf("foreign session GET = %d", status)
	}
	// The other uplink's own stream carries only its scope.
	other, _ := openStream(t, url, "other-token", "", "")
	other.next(t)
	backend.publish(model.AgentEvent{Kind: model.AgentEventRunFinished, Repo: "a", RunID: "hidden", Summary: "hidden"})
	backend.publish(model.AgentEvent{Kind: model.AgentEventRunFinished, Repo: "b", RunID: "visible", Summary: "visible"})
	if message := other.nextNotification(t); message.params["meta"].(map[string]any)["run_id"] != "visible" {
		t.Fatalf("scoped stream = %+v", message)
	}
}

func TestMCPEventStreamBounds(t *testing.T) {
	url, backend, _ := eventServer(t, WithAgentEventStreams(1, 2))
	for range 5 {
		backend.publish(model.AgentEvent{Kind: model.AgentEventRunFinished, Repo: "a", RunID: "r", Summary: "old"})
	}
	session := initializeSession(t, url, "valid-token")
	stream, status := openStream(t, url, "valid-token", session, "1")
	if status != http.StatusOK {
		t.Fatalf("GET = %d", status)
	}
	stream.next(t)
	truncated := stream.nextNotification(t)
	if meta := truncated.params["meta"].(map[string]any); meta["kind"] != "events.backlog_truncated" || meta["after_id"] != "1" {
		t.Fatalf("backlog notice = %+v", truncated)
	}
	if first, second := stream.nextNotification(t), stream.nextNotification(t); first.id != "4" || second.id != "5" {
		t.Fatalf("replayed %s %s, want the newest 2", first.id, second.id)
	}
	// One stream per credential: a second is refused, another credential is not.
	if _, status := openStream(t, url, "valid-token", "", ""); status != http.StatusTooManyRequests {
		t.Fatalf("second stream = %d", status)
	}
	if other, status := openStream(t, url, "other-token", "", ""); status != http.StatusOK || !other.next(t).comment {
		t.Fatalf("other credential stream = %d", status)
	}
	// Heartbeats keep an idle stream alive.
	if heartbeat := stream.next(t); !heartbeat.comment {
		t.Fatalf("heartbeat = %+v", heartbeat)
	}
	// A GET that does not accept SSE is refused.
	request, _ := http.NewRequest(http.MethodGet, url+"/mcp", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	if response, err := http.DefaultClient.Do(request); err != nil || response.StatusCode != http.StatusNotAcceptable {
		t.Fatalf("plain GET = %v %v", response, err)
	}
}

func TestMCPWithoutEventFeedHasNoPush(t *testing.T) {
	server, _ := testServer(t)
	request := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	request.Header.Set("Accept", "text/event-stream")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET without feed = %d", response.Code)
	}
	initialize := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`))
	initialize.Header.Set("Authorization", "Bearer valid-token")
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, initialize)
	if response.Header().Get(mcpSessionHeader) != "" || strings.Contains(response.Body.String(), `"experimental"`) {
		t.Fatalf("initialize without feed = %s", response.Body.String())
	}
}

// A server that records agent events teaches the one-call loop in its
// initialize instructions: events_wait kept open with own, the two kinds and
// the cursor, deduplicated by event id.
func TestMCPInitializeTeachesTheEventsWaitLoop(t *testing.T) {
	url, _, _ := eventServer(t)
	_, decoded := postMCP(t, url, "valid-token", "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`)
	result, _ := decoded["result"].(map[string]any)
	instructions, _ := result["instructions"].(string)
	for _, want := range []string{
		"keep one events_wait {kinds:[run.held,run.finished], own:true, after_id, timeout_s:600} open",
		"next_after_id as after_id", "skip any id at or below the highest one handled", "never wait for them", "abandon_at",
	} {
		if !strings.Contains(instructions, want) {
			t.Errorf("initialize instructions lack %q:\n%s", want, instructions)
		}
	}
}
