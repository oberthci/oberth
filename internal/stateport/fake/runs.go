package fake

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

// Runs is an in-memory implementation of the service's durable run/query ports.
// A mutation stages a copy and publishes one change notification after success.
type Runs struct {
	// Before observes or refuses a fixture operation before any state changes.
	Before  func(string, bool) error
	mu      sync.Mutex
	changed chan struct{}
	clock   func() time.Time
	data    runMemory
}

type runReceipt struct {
	Binding string
	IDs     []string
}
type runMemory struct {
	now           time.Time
	upstreams     map[string]stateport.Upstream
	grants        map[string]stateport.GrantChange
	grantRevision int64
	issues        map[int64]model.Issue
	ciSequences   map[string]int64
	ciCounted     map[string]bool
	issueID       int64
	runs          map[string]stateport.Run
	receipts      map[string]runReceipt
	receivedHeads map[string]model.CIRequest
	repos         map[string]stateport.Repository
	obligations   map[string]stateport.Obligation

	promotions        map[string]model.Promotion
	native            map[string]nativeEvidence
	purges            map[string]stateport.PurgeReceipt
	publications      map[string]model.Publication
	events            []model.AgentEvent
	dedupe            map[string]model.AgentEvent
	audits            []model.AuditAction
	pruned            map[string]int64
	sequence, eventID int64
}

func NewRuns(clock func() time.Time) *Runs {
	return &Runs{clock: clock, changed: make(chan struct{}), data: runMemory{upstreams: map[string]stateport.Upstream{}, grants: map[string]stateport.GrantChange{}, issues: map[int64]model.Issue{}, ciSequences: map[string]int64{}, ciCounted: map[string]bool{}, runs: map[string]stateport.Run{}, receipts: map[string]runReceipt{}, receivedHeads: map[string]model.CIRequest{},
		repos: map[string]stateport.Repository{}, obligations: map[string]stateport.Obligation{}, promotions: map[string]model.Promotion{},
		native: map[string]nativeEvidence{}, pruned: map[string]int64{}, purges: map[string]stateport.PurgeReceipt{}, publications: map[string]model.Publication{}, dedupe: map[string]model.AgentEvent{}}}
}

func (m runMemory) clone() runMemory {
	m.upstreams = maps.Clone(m.upstreams)
	m.native = maps.Clone(m.native)
	m.grants = maps.Clone(m.grants)
	m.issues = maps.Clone(m.issues)
	m.ciSequences = maps.Clone(m.ciSequences)
	m.ciCounted = maps.Clone(m.ciCounted)
	m.pruned = maps.Clone(m.pruned)
	m.runs = maps.Clone(m.runs)
	m.receipts = maps.Clone(m.receipts)
	m.receivedHeads = maps.Clone(m.receivedHeads)
	m.repos = maps.Clone(m.repos)
	m.obligations = maps.Clone(m.obligations)
	m.promotions = maps.Clone(m.promotions)
	m.purges = maps.Clone(m.purges)
	m.publications = maps.Clone(m.publications)
	m.dedupe = maps.Clone(m.dedupe)
	m.events = slices.Clone(m.events)
	m.audits = slices.Clone(m.audits)
	return m
}

func (f *Runs) transact(ctx context.Context, events []stateport.EventWrite, apply func(*runMemory) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validEvents(events); err != nil {
		return err
	}
	next := f.data.clone()
	next.now = f.clock().UTC()
	if err := apply(&next); err != nil {
		var unchanged noRunChange
		if errors.As(err, &unchanged) {
			return nil
		}
		return err
	}
	if err := next.appendEvents(events); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f.data = next
	close(f.changed)
	f.changed = make(chan struct{})
	return nil
}

type noRunChange struct{}

func (noRunChange) Error() string { return "unchanged" }
func runProblem(code publicerr.Code) error {
	return publicerr.New(code, "The state operation was refused.", "Read the current state and retry with valid values.", nil)
}
func jsonValue(v any) string { body, _ := json.Marshal(v); return string(body) }
func visibleRun(scope []string, repo string) bool {
	return scope == nil || slices.Contains(scope, repo)
}
func runTerminal(r stateport.Run) bool {
	return r.Status == "passed" || r.Status == "failed" || r.Status == "cancelled"
}

func (f *Runs) Changes() <-chan struct{} { f.mu.Lock(); defer f.mu.Unlock(); return f.changed }

// SeedRepository is used by consumer contract fixtures before their queries.
func (f *Runs) SeedRepository(repo stateport.Repository) {
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := strings.Split(repo.Canonical, "/")
	if repo.Upstream == "" {
		repo.Upstream = parts[0]
	}
	if repo.Name == "" {
		repo.Name = parts[len(parts)-1]
	}
	if repo.ID == 0 {
		repo.ID = int64(len(f.data.repos) + 1)
	}
	f.data.repos[repo.Canonical] = repo
}

func (f *Runs) Audits() []model.AuditAction {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.data.audits)
}
func (m *runMemory) record(actor, name, id, reason string) {
	m.audits = append(m.audits, model.AuditAction{ID: int64(len(m.audits) + 1), Actor: actor, Action: name, ResourceID: id, ResourceType: "run", CreatedAt: m.now, Details: jsonValue(map[string]any{"reason": reason})})
}

func (f *Runs) before(ctx context.Context, operation string, write bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.Before != nil {
		return f.Before(operation, write)
	}
	return nil
}

// SeedNodes supplies raw native observations to read-contract fixtures.
func (f *Runs) SeedNodes(id string, nodes []stateport.Node) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.data.native[id]
	e.Stored = cloneNodes(nodes)
	f.data.native[id] = e
}
