package api

import (
	"context"
	"encoding/json"
	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"reflect"
	"slices"
	"strings"
	"time"
)

type prepared struct {
	meta apicontract.Metadata
	call func(context.Context) (any, error)
}

// Tool is opaque: construction always binds concrete types and their handler.
type Tool struct {
	meta    apicontract.Metadata
	bound   bool
	prepare func(context.Context, apicontract.Caller, []byte) (prepared, error)
}

type Registry struct {
	listings    map[apicontract.Access]json.RawMessage
	maximumWait time.Duration
	entries     map[string]Tool
	events      apicontract.Handler[apicontract.EventsWaitArgs, apicontract.EventResult]
}

// NewRegistry refuses empty, duplicate, unbound and unclassified declarations.
func NewRegistry(entries []Tool) (*Registry, error) {
	r := &Registry{maximumWait: 600 * time.Second, entries: make(map[string]Tool, len(entries))}
	if len(entries) == 0 {
		return nil, refusal("registry")
	}
	for _, e := range entries {
		if !validMetadata(e.meta) || !e.bound || e.prepare == nil {
			return nil, refusal("registry")
		}
		if _, exists := r.entries[e.meta.Name]; exists {
			return nil, refusal("registry")
		}
		r.entries[e.meta.Name] = e
	}
	r.cacheListings()
	return r, nil
}

func validMetadata(m apicontract.Metadata) bool {
	return m.Name != "" && m.Description != "" && m.Access.Valid() &&
		m.InputBytes > 0 && m.OutputBytes > 0 && m.Deadline > 0
}

func bindDefinition(d apicontract.Definition) Tool {
	m := d.Metadata
	m.Annotations = apicontract.Annotations{ReadOnly: !m.Mutation, Destructive: m.Mutation}
	m.InputSchema = schemaFor(d.InputType)
	m.OutputSchema = schemaFor(d.ResultType)
	return Tool{meta: m, bound: d.Bound, prepare: func(ctx context.Context, c apicontract.Caller, raw []byte) (prepared, error) {
		input := d.NewInput()
		if err := decode(raw, input, m.InputBytes); err != nil {
			return prepared{}, err
		}
		if err := validateInput(input); err != nil {
			return prepared{}, err
		}
		local := inputMetadata(ctx, m, reflect.ValueOf(input).Elem())
		timeout := d.Timeout(input)
		prepareCtx, cancel := context.WithTimeout(ctx, local.Deadline)
		defer cancel()
		budget := requestBudget(ctx)
		if budget != nil {
			deadline, _ := prepareCtx.Deadline()
			budget.install(deadline)
		}
		call, err := ownedCall(prepareCtx, func() (apicontract.Call[any], error) { return d.Prepare(prepareCtx, c, input) }, func() {
			if budget != nil {
				budget.expire(local, timeout)
			}
		})
		if err != nil {
			return prepared{}, err
		}
		if call == nil {
			return prepared{}, refusal("internal")
		}
		deadline, _ := prepareCtx.Deadline()
		p := prepared{meta: local, call: boundedCall(local, call, deadline, timeout)}
		return p, nil
	}}
}

func nilHandler(h any) bool {
	if h == nil {
		return true
	}
	v := reflect.ValueOf(h)
	return (v.Kind() == reflect.Pointer || v.Kind() == reflect.Func || v.Kind() == reflect.Interface) && v.IsNil()
}

func visible(c apicontract.Caller, m apicontract.Metadata) bool {
	return m.Access.Allows(c.Actor)
}
func metricsOnly(c apicontract.Caller) bool {
	return slices.Contains(c.Actor.Scope, string(apicontract.Metrics))
}

// Invoke is the same bounded typed path used by HTTP aliases and MCP.
func (r *Registry) Invoke(ctx context.Context, c apicontract.Caller, name string, raw []byte) (any, error) {
	ctx = context.WithValue(ctx, waitLimitKey{}, r.maximumWait)
	p, err := r.prepare(ctx, c, name, raw)
	if err != nil {
		return nil, err
	}
	return p.call(ctx)
}
func (r *Registry) prepare(ctx context.Context, c apicontract.Caller, name string, raw []byte) (prepared, error) {
	e, ok := r.entries[name]
	if !ok {
		return prepared{}, refusal("unknown")
	}
	if !visible(c, e.meta) {
		return prepared{}, refusal("forbidden")
	}
	return e.prepare(context.WithValue(ctx, waitLimitKey{}, r.maximumWait), c, raw)
}

type waitLimitKey struct{}

func maximumWait(ctx context.Context) time.Duration {
	if v, ok := ctx.Value(waitLimitKey{}).(time.Duration); ok {
		return v
	}
	return 600 * time.Second
}
func boundedCall[O any](m apicontract.Metadata, call apicontract.Call[O], deadline time.Time, timeout O) func(context.Context) (any, error) {
	return func(ctx context.Context) (any, error) {
		ctx, cancel := context.WithDeadline(ctx, deadline)
		defer cancel()
		budget := requestBudget(ctx)
		value, err := ownedCall(ctx, func() (O, error) { return call(ctx) }, func() {
			if budget != nil {
				budget.expire(m, timeout)
			}
		})
		return value, err
	}
}

// Wire catalogues are encoded once; listing a tool never clones or encodes schemas.
func (r *Registry) cacheListings() {
	r.listings = map[apicontract.Access]json.RawMessage{}
	for _, access := range []apicontract.Access{apicontract.Read, apicontract.Admin, apicontract.ScopedAdmin} {
		caller := actor.Actor{Admin: access != apicontract.Read}
		if access == apicontract.ScopedAdmin {
			caller.Scope = []string{"repository"}
		}
		tools := []apicontract.Metadata{}
		for _, entry := range r.entries {
			if entry.meta.Access.Allows(caller) {
				tools = append(tools, entry.meta)
			}
		}
		slices.SortFunc(tools, func(a, b apicontract.Metadata) int { return strings.Compare(a.Name, b.Name) })
		data, _ := json.Marshal(struct {
			Tools []apicontract.Metadata `json:"tools"`
		}{tools})
		r.listings[access] = data
	}
}
func (r *Registry) listWire(c apicontract.Caller) json.RawMessage {
	if metricsOnly(c) {
		return json.RawMessage(`{"tools":[]}`)
	}
	access := apicontract.Read
	if c.Actor.Admin {
		access = apicontract.Admin
		if len(c.Actor.Scope) > 0 {
			access = apicontract.ScopedAdmin
		}
	}
	return r.listings[access]
}

func inputMetadata(ctx context.Context, m apicontract.Metadata, input reflect.Value) apicontract.Metadata {
	apicontract.ApplyDefaults(input)
	if m.Tail {
		logDefaults(input)
	}
	local := m
	maximum := time.Duration(1<<63 - 1)
	if m.LongPoll {
		maximum = maximumWait(ctx)
	}
	if timeout := apicontract.InputTimeout(input, maximum); timeout > 0 {
		local.Deadline = timeout
		if m.LongPoll {
			local.Deadline += apicontract.WaitMargin
		}
	}
	return local
}
