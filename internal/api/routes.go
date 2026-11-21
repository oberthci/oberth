package api

import (
	"encoding/json"
	"github.com/oberthci/oberth/internal/apicontract"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func (s *Server) readRoutes() {
	for name, t := range s.config.Registry.entries {
		if t.meta.Get != "" {
			s.mux.HandleFunc("GET "+t.meta.Get, s.private(s.read(name)))
		}
		if t.meta.Post != "" {
			s.mux.HandleFunc("POST "+t.meta.Post, s.private(s.action(name)))
		}
	}

	for _, d := range apicontract.ViewDefinitions(s.config.Views) {
		s.mux.HandleFunc("GET "+d.Get, s.private(s.readView(bindDefinition(d))))
	}
}
func (s *Server) read(name string) endpoint {
	return func(w http.ResponseWriter, r *http.Request, c apicontract.Caller) {
		t := s.config.Registry.entries[name]
		raw, err := queryObject(r, t.meta.InputSchema)
		if err != nil {
			s.writeError(w, r, err, 400)
			return
		}
		ctx := s.httpBudget(w, r)
		value, err := s.config.Registry.Invoke(ctx, c, name, raw)
		if requestBudget(ctx).expired {
			return
		}
		if err != nil {
			s.writeError(w, r, err, 0)
			return
		}
		s.writeJSON(w, r, jsonReply{200, value, t.meta.OutputBytes})
	}
}
func (s *Server) readView(t Tool) endpoint {
	return func(w http.ResponseWriter, r *http.Request, c apicontract.Caller) {
		if !t.bound || metricsOnly(c) {
			s.writeError(w, r, refusal("forbidden"), 403)
			return
		}
		raw, err := queryObject(r, t.meta.InputSchema)
		if err != nil {
			s.writeError(w, r, err, 400)
			return
		}
		ctx := s.httpBudget(w, r)
		p, err := t.prepare(ctx, c, raw)
		if requestBudget(ctx).expired {
			return
		}
		if err != nil {
			s.writeError(w, r, err, 0)
			return
		}
		value, err := p.call(ctx)
		if requestBudget(ctx).expired {
			return
		}
		if err != nil {
			s.writeError(w, r, err, 0)
			return
		}
		s.writeJSON(w, r, jsonReply{200, value, apicontract.ResponseBytes})
	}
}
func queryObject(r *http.Request, schema apicontract.Schema) ([]byte, error) {
	query, err := parseQuery(r)
	if err != nil {
		return nil, err
	}
	obj := map[string]any{}
	properties, _ := schema["properties"].(apicontract.Schema)
	for k, values := range query {
		prop, ok := properties[k].(apicontract.Schema)
		if !ok || len(values) != 1 {
			return nil, argumentError(k)
		}
		value, err := queryValue(values[0], prop)
		if err != nil {
			return nil, argumentError(k)
		}
		obj[k] = value
	}
	if id := r.PathValue("run"); id != "" {
		obj["id"] = id
	}
	if id := r.PathValue("id"); id != "" {
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			return nil, argumentError("id")
		}
		obj["id"] = n
	}
	return json.Marshal(obj)
}
func queryValue(value string, schema apicontract.Schema) (any, error) {
	if variants, ok := schema["anyOf"].([]apicontract.Schema); ok {
		schema = variants[0]
	}
	switch schema["type"] {
	case "boolean":
		return strconv.ParseBool(value)
	case "integer":
		return strconv.ParseInt(value, 10, 64)
	case "array":
		return strings.Split(value, ","), nil
	}
	return value, nil
}
func (s *Server) action(name string) endpoint {
	return func(w http.ResponseWriter, r *http.Request, c apicontract.Caller) {
		if !visible(c, s.config.Registry.entries[name].meta) {
			s.writeError(w, r, refusal("forbidden"), 403)
			return
		}
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" || r.URL.RawQuery != "" {
			s.writeError(w, r, refusal("media"), 400)
			return
		}
		limit := s.config.Registry.entries[name].meta.InputBytes
		raw, err := readBody(w, r, limit)
		defer clear(raw)
		if err != nil {
			s.writeError(w, r, err, bodyStatus(err))
			return
		}
		ctx := s.httpBudget(w, r)
		if name == "secret_put" {
			var body apicontract.SecretPutBody
			err = decode(raw, &body, apicontract.SecretBodyBytes)
			defer clear(body.Value)
			if err != nil || body.Value == nil {
				s.writeError(w, r, refusal("input"), 400)
				return
			}
			raw, _ = json.Marshal(body.SecretPutArgs)
			ctx = apicontract.WithSecretValue(ctx, body.Value)
		}
		value, err := s.config.Registry.Invoke(ctx, c, name, raw)
		if requestBudget(ctx).expired {
			return
		}
		if err != nil {
			s.writeError(w, r, err, 0)
			return
		}
		s.writeJSON(w, r, jsonReply{200, value, apicontract.ResponseBytes})
	}
}
func (s *Server) admin(w http.ResponseWriter, r *http.Request, c apicontract.Caller) {
	action := r.PathValue("action")
	if action == "database-backup" {
		s.backup(w, r, c)
		return
	}
	for name, t := range s.config.Registry.entries {
		if t.meta.AdminAction == action {
			s.action(name)(w, r, c)
			return
		}
	}
	s.writeError(w, r, refusal("unknown"), 404)
}

func parseQuery(r *http.Request) (url.Values, error) {
	if len(r.URL.RawQuery) > apicontract.RequestBytes {
		return nil, refusal("input")
	}
	v, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, refusal("input")
	}
	return v, nil
}
