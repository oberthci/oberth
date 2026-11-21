package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/oberthci/oberth/internal/apicontract"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/oberthci/oberth/internal/publicerr"
)

func (s *Server) publicError(err error, traces ...*Diagnostic) *publicerr.Error {
	if s.draining.Load() && errors.Is(err, context.Canceled) {
		return refusal("restarting")
	}
	e := s.config.Classify(err)
	if e == nil || e.Code() == publicerr.Invariant {
		e = refusal("internal")
		reference := s.failureReference(traces)
		e.Detail = "Reference: " + reference
		s.config.FailureLog.Error("Request failed", "reference", reference, "cause", err)
	}
	return e
}
func httpStatus(e *publicerr.Error) int {
	switch e.Code() {
	case publicerr.NotFound:
		return 404
	case publicerr.Forbidden:
		return 403
	case publicerr.InvalidInput, publicerr.SchemaRefused:
		return 400
	case publicerr.Conflict, publicerr.InvalidTransition, publicerr.Ambiguous:
		return 409
	case publicerr.Unavailable:
		return 503
	}
	return 500
}
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error, status int) {
	e := s.publicError(err)
	if status == 0 {
		status = httpStatus(e)
	}
	if status == 503 {
		w.Header().Set("Retry-After", "1")
	}
	s.writeJSON(w, r, jsonReply{status, e, apicontract.ResponseBytes})
}

type jsonReply struct {
	status int
	value  any
	limit  int
}

func (s *Server) writeJSON(w http.ResponseWriter, r *http.Request, reply jsonReply) {
	status, value, limit := reply.status, reply.value, reply.limit
	data, err := encodeBounded(value, limit)
	if err != nil {
		status = 500
		data, _ = json.Marshal(s.publicError(err))
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Vary", "Accept-Encoding")
	if r.Method == "GET" && status == 200 {
		sum := sha256.Sum256(data)
		tag := fmt.Sprintf("W/\"%x\"", sum)
		w.Header().Set("ETag", tag)
		if etagMatches(r.Header.Get("If-None-Match"), tag) {
			w.WriteHeader(304)
			return
		}
	}
	if acceptsGzip(r.Header.Get("Accept-Encoding")) {
		var buf bytes.Buffer
		z := gzip.NewWriter(&buf)
		_, _ = z.Write(data)
		_ = z.Close()
		data = buf.Bytes()
		w.Header().Set("Content-Encoding", "gzip")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
func encodeBounded(value any, limit int) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, refusal("output")
	}
	return data, nil
}
func etagMatches(header, tag string) bool {
	for _, v := range strings.Split(header, ",") {
		v = strings.TrimSpace(v)
		if v == "*" || strings.TrimPrefix(v, "W/") == strings.TrimPrefix(tag, "W/") {
			return true
		}
	}
	return false
}
func acceptsGzip(header string) bool { return acceptsMedia([]string{header}, "gzip") }
func readBody(w http.ResponseWriter, r *http.Request, limit int) ([]byte, error) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(limit)))
	if err != nil {
		var oversized *http.MaxBytesError
		if errors.As(err, &oversized) {
			m := messages()["bodySize"]
			e := publicerr.New(m.code, m.reason, m.remedy, err)
			e.Detail = "Limit: " + strconv.Itoa(limit) + " bytes"
			return nil, e
		}
		return nil, refusal("bodyRead")
	}
	return data, nil
}

func bodyStatus(err error) int {
	var oversized *http.MaxBytesError
	if errors.As(err, &oversized) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}
