package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

func (server *Server) handleRunContainerLogs(writer http.ResponseWriter, request *http.Request) {
	id := request.PathValue("run")
	if !validRunSelector(id) {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "run ID is required"})
		return
	}
	value, err := server.views.RunContainerLogs(request.Context(), actorFrom(request.Context()), id)
	server.writeView(writer, value, err)
}

func containerLogSelector(writer http.ResponseWriter, request *http.Request) (string, string, string, bool) {
	id, node, container := request.PathValue("run"), request.URL.Query().Get("node_id"), request.URL.Query().Get("container")
	if !validRunSelector(id) || !validNativeLogSelector(node) || (container != "" && !validNativeLogSelector(container)) {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "run, node_id and container must be valid selectors"})
		return "", "", "", false
	}
	return id, node, container, true
}

func (server *Server) handleRunContainerLog(writer http.ResponseWriter, request *http.Request) {
	id, node, container, ok := containerLogSelector(writer, request)
	if !ok {
		return
	}
	filter, err := logFilterFrom(request.URL.Query())
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	value, err := server.views.RunContainerLogFiltered(request.Context(), actorFrom(request.Context()), id, node, container, filter)
	server.writeView(writer, value, err)
}

func (server *Server) handleRunContainerLogStream(writer http.ResponseWriter, request *http.Request) {
	id, node, container, ok := containerLogSelector(writer, request)
	if !ok {
		return
	}
	follow := true
	if value := request.URL.Query().Get("follow"); value != "" {
		var err error
		follow, err = strconv.ParseBool(value)
		if err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "follow must be true or false"})
			return
		}
	}
	if follow && container == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "container is required for a live stream"})
		return
	}
	if !supportsMCPFlush(writer) {
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "streaming unavailable"})
		return
	}
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	stream, err := server.views.OpenRunContainerLog(ctx, actorFrom(ctx), id, node, container, follow)
	if err != nil {
		server.writeView(writer, nil, err)
		return
	}
	closeStream := sync.OnceFunc(func() { _ = stream.Close() })
	defer closeStream()
	stopClose := context.AfterFunc(ctx, closeStream)
	defer stopClose()
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Accel-Buffering", "no")
	response := http.NewResponseController(writer)
	_ = response.SetWriteDeadline(time.Time{})
	if err := response.Flush(); err != nil {
		return
	}
	stopWrite := context.AfterFunc(ctx, func() { _ = response.SetWriteDeadline(time.Now()) })
	defer func() { stopWrite(); _ = response.SetWriteDeadline(time.Time{}) }()
	joined := make(chan struct{})
	go func() { defer close(joined); server.watchContainerLogAccess(ctx, request, id, cancel) }()
	defer func() { cancel(); closeStream(); <-joined }()
	_, _ = io.Copy(&containerLogHTTPWriter{ctx: ctx, writer: writer, response: response}, stream)
}

func (server *Server) watchContainerLogAccess(ctx context.Context, request *http.Request, id string, cancel context.CancelFunc) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	lastAuth := time.Now()
	original := actorFrom(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		check, stop := context.WithTimeout(ctx, 5*time.Second)
		err := server.views.CheckContainerLogAccess(check, original, id)
		if err == nil && time.Since(lastAuth) >= liveLogAuthInterval {
			fresh, authErr := server.authenticateRequest(check, request)
			if authErr != nil || fresh.Identity != original.Identity || fresh.Fingerprint != original.Fingerprint || fresh.Admin != original.Admin || !slices.Equal(fresh.Scope, original.Scope) {
				err = errors.New("container log access changed")
			}
			lastAuth = time.Now()
		}
		stop()
		if err != nil {
			cancel()
			return
		}
	}
}

type containerLogHTTPWriter struct {
	ctx      context.Context
	writer   http.ResponseWriter
	response *http.ResponseController
}

func (out *containerLogHTTPWriter) Write(body []byte) (int, error) {
	if err := out.ctx.Err(); err != nil {
		return 0, err
	}
	if err := out.response.SetWriteDeadline(time.Now().Add(30 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return 0, err
	}
	n, err := out.writer.Write(body)
	if err == nil {
		err = out.response.Flush()
	}
	_ = out.response.SetWriteDeadline(time.Time{})
	return n, err
}

func validNativeLogSelector(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\t\r\n")
}
