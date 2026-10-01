package vmpilotfixture

import (
	"encoding/json"
	"time"

	"github.com/oberthci/oberth/internal/vmrunner"
)

// Keep this field order aligned with trusted/beacon-conductor/protocol.go in
// the independently owned E2E repository. Only this closed encoder may marshal
// fixture capabilities; a CA signing key has no representation in the schema.
type bootstrapTenant struct {
	ID  string `json:"id"`
	Key []byte `json:"key"`
}

type bootstrap struct {
	Version       int                    `json:"version"`
	Type          string                 `json:"type"`
	Execution     string                 `json:"execution"`
	Attempt       string                 `json:"attempt"`
	Inventory     string                 `json:"inventory"`
	SuiteRevision string                 `json:"suite_revision"`
	TimeoutMillis int64                  `json:"timeout_millis"`
	KeyScope      string                 `json:"key_scope"`
	RootCA        []byte                 `json:"root_ca"`
	Tenants       [2]bootstrapTenant     `json:"tenants"`
	Endpoint      vmrunner.PilotEndpoint `json:"endpoint"`
}

// WithBootstrap lends one canonical bounded frame for the exact conductor
// process and initial endpoint. It is consumed even if delivery fails. The
// future attach adapter must establish Pod/container provenance before this
// call and keep it through termination; this encoder does not establish it.
// The callback must not reenter Fixture methods or retain/copy the frame.
func (f *Fixture) WithBootstrap(attempt vmrunner.ConductorAttempt, endpoint vmrunner.PilotEndpoint, deliver func([]byte) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.available() {
		return ErrUnavailable
	}
	earliestStart := f.created
	if attempt.StartedAt.Nanosecond() == 0 {
		earliestStart = earliestStart.Truncate(time.Second)
	}
	if deliver == nil || f.bootstrapped || endpoint.Attempt != vmrunner.InitialVMResource ||
		endpoint != f.leaves[0].endpoint || vmrunner.ValidateConductorAttempt(f.plan, attempt) != nil ||
		attempt.StartedAt.Before(earliestStart) || !time.Now().Before(attempt.StartedAt.Add(f.plan.Spec.Deadline)) {
		return ErrBinding
	}
	value := bootstrap{Version: 1, Type: "bootstrap", Execution: vmrunner.PilotIdentity(f.plan),
		Attempt: vmrunner.ConductorAttemptIdentity(attempt), Inventory: vmrunner.PilotInventoryIdentity(f.plan),
		SuiteRevision: f.plan.Spec.SuiteRevision, TimeoutMillis: f.plan.Spec.Deadline.Milliseconds(),
		KeyScope: "per-run", RootCA: f.caPEM, Endpoint: endpoint}
	for i, tenant := range f.tenants {
		value.Tenants[i] = bootstrapTenant(tenant)
	}
	body, err := json.Marshal(value)
	if err != nil {
		return ErrBinding
	}
	defer clear(body)
	if len(body)+1 > 32<<10 {
		return ErrBinding
	}
	// A separate exact-sized buffer avoids leaving a pre-growth copy containing
	// synthetic keys when appending the newline to json.Marshal's allocation.
	frame := make([]byte, len(body)+1)
	copy(frame, body)
	frame[len(body)] = '\n'
	defer clear(frame)
	f.bootstrapped = true
	if err := deliver(frame); err != nil {
		return ErrDelivery
	}
	return nil
}
