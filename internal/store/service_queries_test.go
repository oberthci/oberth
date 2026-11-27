package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

type queryProbe struct {
	mu       sync.Mutex
	queries  []string
	prepares []string
}

func (p *queryProbe) record(q string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queries = append(p.queries, q)
}
func (p *queryProbe) take() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.queries
	p.queries = nil
	return out
}

func (p *queryProbe) recordPrepare(query string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prepares = append(p.prepares, query)
}
func (p *queryProbe) takePrepares() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.prepares
	p.prepares = nil
	return out
}

type probeConnector struct {
	driver driver.Driver
	dsn    string
	probe  *queryProbe
}

func (c probeConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return probeConn{Conn: conn, probe: c.probe}, nil
}
func (c probeConnector) Driver() driver.Driver { return c.driver }

type probeConn struct {
	driver.Conn
	probe *queryProbe
}

type probeStmt struct {
	driver.Stmt
	query string
	probe *queryProbe
}

func (c probeConn) Prepare(query string) (driver.Stmt, error) {
	c.probe.recordPrepare(query)
	stmt, err := c.Conn.Prepare(query)
	if err != nil {
		return nil, err
	}
	return probeStmt{Stmt: stmt, query: query, probe: c.probe}, nil
}
func (c probeConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	c.probe.recordPrepare(query)
	stmt, err := c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	return probeStmt{Stmt: stmt, query: query, probe: c.probe}, nil
}
func (s probeStmt) Query(args []driver.Value) (driver.Rows, error) {
	s.probe.record(s.query)
	return s.Stmt.Query(args)
}
func (s probeStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	s.probe.record(s.query)
	return s.Stmt.(driver.StmtQueryContext).QueryContext(ctx, args)
}
func (s probeStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.Stmt.(driver.StmtExecContext).ExecContext(ctx, args)
}
func (c probeConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	c.probe.recordPrepare(q)
	c.probe.record(q)
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
}
func (c probeConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, o)
}

func probeQueries(t *testing.T, s *Store) *queryProbe {
	t.Helper()
	p := new(queryProbe)
	db := sql.OpenDB(probeConnector{driver: s.readers.Driver(), dsn: "file:" + s.path + "?mode=ro", probe: p})
	requireOK(t, s.readers.Close())
	s.readers = db
	return p
}

func TestRunStateOneSummaryQueryPerNativeWake(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	r, _ := accepted(t, s, "waiter")
	p := probeQueries(t, s)
	for range 10 {
		ch := observeChanges(t, s)
		_, err := s.BeginEvidence(t.Context(), stateport.Action{}, stateport.EvidenceStart{RunID: r.ID, Nodes: 1})
		requireOK(t, err)
		select {
		case <-ch:
		default:
			t.Fatal("native fact did not wake waiter")
		}
		state, err := s.RunState(t.Context(), r.ID)
		requireOK(t, err)
		if state.Total != 1 || state.Revision != r.Revision {
			t.Fatal("summary lost normalized counts or revision", state)
		}
	}
	queries := p.take()
	if len(queries) != 10 {
		t.Fatal("waiter performed extra reads", queries)
	}
	for _, q := range queries {
		if strings.Contains(q, "admission") || strings.Contains(q, "nodes") {
			t.Fatal("waiter decoded evidence", q)
		}
	}
	_, err := s.writer.ExecContext(t.Context(), `UPDATE runs SET admission='invalid' WHERE id=?`, r.ID)
	requireOK(t, err)
	_, err = s.RunState(t.Context(), r.ID)
	requireOK(t, err)
	if _, err = s.Run(t.Context(), r.ID); err == nil {
		t.Fatal("corrupt admission control did not fire")
	}
}

func TestActorEventPageUsesOneIndexedDataQuery(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	r, _ := accepted(t, s, "events")
	events := make([]stateport.EventWrite, 500)
	for i := range events {
		events[i] = atomicEvent(s, fmt.Sprint("actor-", i))
		events[i].Event.RunID = r.ID
	}
	requireOK(t, s.RecordAction(t.Context(), model.AuditAction{Actor: "operator", Action: "fixture", ResourceType: "event"}, events...))
	p := probeQueries(t, s)
	page, err := s.Events(t.Context(), stateport.EventFilter{Actor: "operator", Limit: 500})
	requireOK(t, err)
	if len(page.Events) != 500 {
		t.Fatal("actor page incomplete")
	}
	queries := p.take()
	data := 0
	for _, q := range queries {
		if strings.Contains(q, "SELECT e.id,e.value") {
			data++
			if !strings.Contains(q, "run_actor") || !strings.Contains(q, "event_run") {
				t.Fatal("actor query lost indexed join", q)
			}
		}
	}
	if data != 1 || len(queries) > 5 {
		t.Fatal("actor page repeated reads", queries)
	}
	query, args := eventQuery(stateport.EventFilter{Actor: "operator", Limit: 500}, 1000)
	plan := boundQueryPlan(t, s, query, args)
	if !strings.Contains(plan, "SEARCH r USING INDEX run_actor") || !strings.Contains(plan, "SEARCH e USING INDEX event_run") {
		t.Fatal("actor query scans facts", plan)
	}
}

func TestRepositoryScopeUsesOneCatalogQuery(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	atomicRepository(t, s)
	p := probeQueries(t, s)
	names, err := s.RepositoryScope(t.Context(), []string{"origin", "unknown"})
	requireOK(t, err)
	if len(names) != 1 || names[0] != "origin/team/repo" || len(p.take()) != 1 {
		t.Fatal("scope was not one catalogue lookup", names)
	}
}

func boundQueryPlan(t *testing.T, s *Store, q string, args []any) string {
	t.Helper()
	rows, err := s.readers.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+q, args...)
	requireOK(t, err)
	defer func() { requireOK(t, rows.Close()) }()
	var result strings.Builder
	for rows.Next() {
		var a, b, c int
		var detail string
		requireOK(t, rows.Scan(&a, &b, &c, &detail))
		result.WriteString(detail)
		result.WriteByte('\n')
	}
	requireOK(t, rows.Err())
	return result.String()
}
