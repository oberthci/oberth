package store

import (
	"cmp"
	"context"
	"database/sql"
	"slices"
	"strings"
	"sync"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

// issueFact is what list totals read from one issue revision. Work status and
// kind keep only JSON strings; julianday is computed by the database, so the
// stale rule compares the same doubles as the scan.
type issueFact struct {
	id, revision          int64
	upstream, repo, state string
	workStatus, kind      string
	updated               sql.NullFloat64
	malformed             bool
}

// issueFacts is an in-process cache of every issue's facts, ordered by ID; it
// is never persisted. Writers replace facts under mu as they commit, and a
// reader pins its snapshot under mu, so the copy it takes matches that
// snapshot for every write of this process. Each snapshot's audit head and
// issue IDs and revisions are compared with the cache to catch writes of
// other processes: every audited write moves the head, and every unaudited
// issue write adds an ID or a revision.
type issueFacts struct {
	mu         sync.Mutex
	facts      []issueFact // immutable once stored; nil when unknown
	audit      int64       // the audit head the facts were read at or kept to
	generation uint64
}

const issueFactColumns = `SELECT id, revision, upstream, repo, state, ok,
 CASE WHEN ok THEN json_extract(value,'$.work_status') END,
 CASE WHEN ok THEN json_extract(value,'$.kind') END,
 CASE WHEN ok THEN julianday(json_extract(value,'$.updated_at')) END
FROM (SELECT id, revision, upstream, repo, state, value,
 typeof(upstream)='text' AND typeof(repo)='text' AND typeof(state)='text' AND json_valid(value) AS ok FROM issues`

// readIssueFacts reads the facts of the given issues (nil: all), ordered by
// ID. A malformed record is marked instead of failing the read.
func readIssueFacts(ctx context.Context, tx *sql.Tx, ids []int64) ([]issueFact, error) {
	query, args := issueFactColumns+`)`, []any{}
	if ids != nil {
		query = issueFactColumns + ` WHERE id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `))`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	rows, err := tx.QueryContext(ctx, query+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	facts := []issueFact{}
	for rows.Next() {
		var f issueFact
		var ok bool
		var status, kind any
		if err := rows.Scan(&f.id, &f.revision, &f.upstream, &f.repo, &f.state, &ok, &status, &kind, &f.updated); err != nil {
			return nil, err
		}
		f.malformed = !ok
		f.workStatus, _ = status.(string)
		f.kind, _ = kind.(string)
		facts = append(facts, f)
	}
	return facts, rows.Err()
}

// noteIssueWrite records an issue written by the open write transaction; the
// write slot orders these accesses.
func (s *Store) noteIssueWrite(id int64) {
	s.issueWrites = append(s.issueWrites, id)
}

// noteAudit records the audit head before and after an audit row of the open
// write transaction; the write slot orders these accesses.
func (s *Store) noteAudit(before, after int64) {
	if s.auditSpan[1] == 0 {
		s.auditSpan[0] = before
	}
	s.auditSpan[1] = after
}

// commitWrite commits the write transaction and, when it wrote issues or
// audit rows, replaces their facts and audit head in the same critical
// section. Any doubt drops the cache for a rebuild: a failed fact read, an
// uncertain commit, or an audit head that another process moved.
func (s *Store) commitWrite(ctx context.Context, tx *sql.Tx) error {
	ids, audit := s.issueWrites, s.auditSpan
	s.issueWrites, s.auditSpan = nil, [2]int64{}
	if len(ids) == 0 && audit[1] == 0 {
		return tx.Commit()
	}
	var written []issueFact
	var readErr error
	if len(ids) > 0 {
		written, readErr = readIssueFacts(ctx, tx, ids)
	}
	c := &s.issueFacts
	c.mu.Lock()
	defer c.mu.Unlock()
	err := tx.Commit()
	c.generation++
	if err != nil || readErr != nil || c.facts == nil || audit[1] != 0 && c.audit != audit[0] {
		c.facts = nil
		return err
	}
	if audit[1] != 0 {
		c.audit = audit[1]
	}
	touched := map[int64]bool{}
	for _, id := range ids {
		touched[id] = true
	}
	facts := slices.DeleteFunc(slices.Clone(c.facts), func(f issueFact) bool { return touched[f.id] })
	facts = append(facts, written...)
	slices.SortFunc(facts, func(a, b issueFact) int { return cmp.Compare(a.id, b.id) })
	c.facts = facts
	return nil
}

func countIssue(c *model.IssueCounts, v *issueFact, stale bool) {
	switch v.state {
	case string(model.IssueOpen):
		c.Open++
		switch model.IssueWorkStatus(v.workStatus) {
		case model.IssueTodo:
			c.Todo++
		case model.IssueActive:
			c.Active++
		case model.IssueWaiting:
			c.Waiting++
		case model.IssueReview:
			c.Review++
		}
	case string(model.IssueClosed):
		c.Closed++
	}
	if stale {
		c.Stale++
	}
}

// snapshotFacts pins the read snapshot and returns the facts that match it
// with the stale cutoffs as julian days; false sends the read to the scan.
func (s *Store) snapshotFacts(ctx context.Context, tx *sql.Tx, cutoffs []any) ([]issueFact, [2]sql.NullFloat64, bool) {
	var cut [2]sql.NullFloat64
	var head int64
	c := &s.issueFacts
	c.mu.Lock()
	err := tx.QueryRowContext(ctx, `SELECT julianday(?), julianday(?), (SELECT id FROM audit_head WHERE singleton=1)`, cutoffs...).Scan(&cut[0], &cut[1], &head)
	facts, audit, generation := c.facts, c.audit, c.generation
	c.mu.Unlock()
	if err != nil {
		return nil, cut, false
	}
	if audit != head {
		facts = nil
	}
	if same, err := sameIssueKeys(ctx, tx, facts); err != nil || same {
		return facts, cut, err == nil
	}
	if facts, err = readIssueFacts(ctx, tx, nil); err != nil {
		return nil, cut, false
	}
	c.mu.Lock()
	if c.generation == generation {
		c.facts, c.audit = facts, head
	}
	c.mu.Unlock()
	return facts, cut, true
}

// sameIssueKeys compares the snapshot's issue IDs and revisions with facts.
func sameIssueKeys(ctx context.Context, tx *sql.Tx, facts []issueFact) (bool, error) {
	if facts == nil {
		return false, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, revision FROM issues ORDER BY id`)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	i := 0
	for rows.Next() {
		var id, revision int64
		if err := rows.Scan(&id, &revision); err != nil {
			return false, err
		}
		if i == len(facts) || facts[i].id != id || facts[i].revision != revision {
			return false, nil
		}
		i++
	}
	return i == len(facts), rows.Err()
}

// cachedTotals applies the scan's totals rules to facts. It declines a text
// query and a malformed issue in the set, which only the scan reports.
func (l issueList) cachedTotals(page *stateport.IssuePage, facts []issueFact, cut [2]sql.NullFloat64, in func(*issueFact) bool) bool {
	newer := newerThan(page.Issues, l.filter)
	var c model.IssueCounts
	total, offset := 0, 0
	for i := range facts {
		v := &facts[i]
		listed, known := l.lists(v, in)
		if !known {
			return false
		}
		if !listed {
			continue
		}
		stale := v.stale(cut)
		countIssue(&c, v, stale)
		if l.selects(v, stale) {
			total++
			if v.id > newer {
				offset++
			}
		}
	}
	page.Counts, page.Total, page.Offset = c, total, offset
	return true
}

// lists is the set rule: scope, repository, then kind; a malformed record in
// scope and repository is unknown.
func (l issueList) lists(v *issueFact, in func(*issueFact) bool) (listed, known bool) {
	if !in(v) || l.filter.Repo != "" && v.repo != l.filter.Repo {
		return false, true
	}
	if v.malformed {
		return false, false
	}
	return l.filter.Kind == "" || v.kind == string(l.filter.Kind), true
}

// selects is the selection rule over a listed record.
func (l issueList) selects(v *issueFact, stale bool) bool {
	f := l.filter
	state := f.State == "" || f.State == "all" || v.state == f.State
	return state && (f.WorkStatus == "" || v.workStatus == string(f.WorkStatus)) && (!f.Stale || stale)
}

// stale is issueSet's stale rule; cut holds the active and general cutoffs.
func (v *issueFact) stale(cut [2]sql.NullFloat64) bool {
	window := cut[1]
	if model.IssueWorkStatus(v.workStatus) == model.IssueActive {
		window = cut[0]
	}
	return v.state == "open" && v.updated.Valid && window.Valid && v.updated.Float64 <= window.Float64
}
