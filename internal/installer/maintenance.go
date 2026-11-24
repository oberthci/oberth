package installer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/stateport"
	"io/fs"
)

type Maintenance struct {
	Engine       *Engine
	ReleaseFiles func(context.Context, string) (fs.FS, error)
	LiveState    func(context.Context) (stateport.Options, error)
	Source       SnapshotPort
	ActiveRuns   func(context.Context, string) (int, error)
	Options      stateport.Options
}
type UpgradeOptions struct {
	NoBackup bool
}
type UpgradePlan struct {
	repair   *Record
	complete *Record
	Plan     *Plan
	record   Record
	options  UpgradeOptions
}

// Record is the local durable recovery authority for one reviewed transition.
type Record struct {
	Operation      string            `json:"operation"`
	Context        string            `json:"context"`
	ClusterUID     string            `json:"cluster_uid"`
	Namespace      string            `json:"namespace"`
	Source         Revision          `json:"source"`
	TargetVersion  string            `json:"target_version"`
	TargetRevision int               `json:"target_revision"`
	TargetSchema   int               `json:"target_schema"`
	Options        stateport.Options `json:"options"`
	BackupPath     string            `json:"backup_path,omitempty"`
	Archive        *Archive          `json:"archive,omitempty"`
	State          string            `json:"state"`
}

func (m Maintenance) PlanUpgrade(ctx context.Context, r Release, c Choices, o UpgradeOptions) (*UpgradePlan, error) {
	p, repair, count, err := m.prepareUpgrade(ctx, r, c, o)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*UpgradePlan, error) { p.Close(); return nil, err }
	complete, repair, err := m.upgradeRoute(ctx, p, r, repair)
	if err != nil {
		return fail(err)
	}
	if complete != nil {
		return complete, nil
	}

	record := Record{State: "pending", Operation: p.Digest(), Context: c.Context, ClusterUID: p.uid, Namespace: p.choices.Namespace, Source: p.history.baseline(),
		TargetVersion: r.Version(), TargetRevision: p.history.Latest.Number + 1, TargetSchema: r.schema(), Options: m.Options}
	record.Source.Values = p.history.Choices.Values
	if repair != nil {
		if !rollbackLineage(*repair, p.history.Latest) {
			return fail(changedRevision(p.history.Latest.Number, nil))
		}
		record.Options = repair.Options
		record.Source = repair.Source
	}
	if !o.NoBackup {
		record.BackupPath = m.upgradeBackupPath(record)
	}
	p.review = review(p, record.Source.Version+" -> "+r.Version())
	if settingText(p.request.Values, "secretStore", "bundled") == "on" {
		p.review = append(p.review, "If the store restarts sealed, run oberth unseal, then repeat oberth upgrade.")
	}
	if err = m.Engine.reviewPendingStore(p); err != nil {
		return fail(err)
	}
	p.review = append(p.review, upgradeRunsReview(count),
		"Log backfill: one-time storage up to 64 MiB per finished run.",
		"Rollback and uninstall release archive finalizers first; preserve needed logs before either.",
		fmt.Sprintf("Backup: %t; rollback record is saved before changes.", !o.NoBackup))
	if record.BackupPath != "" {
		// The file is named when the apply starts it; the review can only name its directory.
		p.review = append(p.review, "Pre-upgrade backup will be written to "+m.Engine.Credentials.path+"/")
	}
	if repair != nil {
		p.review = append(p.review, "Repair: roll back the interrupted change, then apply this release.")
	}
	p.rehash()
	return &UpgradePlan{Plan: p, record: record, options: o, repair: repair}, nil
}
func (r Record) ID() string {
	raw, _ := json.Marshal(struct {
		UID, Namespace     string
		Version, Operation string
		From, To           int
	}{r.ClusterUID, r.Namespace, r.TargetVersion, r.Operation, r.Source.Number, r.TargetRevision})
	return publicDigest(raw)[:24]
}
func (m Maintenance) Upgrade(ctx context.Context, p *UpgradePlan, approved string, progress Progress) (result Result, err error) {
	if err = m.validateApproval(p, approved); err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, m.Engine.chartLimit()+m.Engine.waitLimit()+2*time.Minute)
	defer cancel()
	unlock, e := m.Engine.Credentials.lock()
	if e != nil {
		return result, e
	}
	defer unlock()
	if p.complete != nil && p.complete.TargetVersion == p.record.TargetVersion {
		return m.completeUpgrade(ctx, p)
	}
	record, err := m.beginUpgrade(ctx, p)
	if err != nil {
		return result, err
	}
	result, err = m.Engine.apply(ctx, p.Plan, approved, progress)
	if err != nil {
		if record.Options.Schema != record.TargetSchema {
			return result, m.schemaRecovery("upgrade-schema-failed", err, record)
		}
		return result, described("upgrade-failed", err, "Rollback record: "+m.Engine.Credentials.pathFor("rollback.json")+"\n"+upgradeCause(err))
	}
	record.State = "completed"
	if err = m.saveRecord(record); err != nil {
		return result, err
	}
	result.Upgrade = UpgradeCompletion{From: record.Source.Version, To: record.TargetVersion, Revision: record.TargetRevision}
	m.finishUpgradeBackups(&result, record)
	return result, nil
}
func (m Maintenance) validateApproval(p *UpgradePlan, approved string) error {
	if p == nil || p.Plan == nil || approved == "" || approved != p.Plan.Digest() {
		return problem("review", nil)
	}
	if m.Engine == nil || !m.Engine.configured() {
		return problem("wiring", nil)
	}
	return nil
}

// discardUnrecorded removes the completed owner-named archive not yet in its record.
func (m Maintenance) discardUnrecorded(path string) error {
	f, err := m.Engine.Credentials.root.OpenFile(strings.TrimPrefix(path, m.Engine.Credentials.path+"/"), 0, 0)
	if isAbsent(err) {
		return nil
	}
	if err != nil {
		return problem("custody", err)
	}
	st, err := f.Stat()
	_ = f.Close()
	if err != nil || !private(st, false) {
		return problem("custody", err)
	}
	return m.Engine.Credentials.root.Remove(st.Name())
}
func (m Maintenance) saveRecord(r Record) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	c := m.Engine.Credentials
	name := "record-" + publicDigest(raw) + ".json"
	if err = c.save(name, raw); err != nil {
		return err
	}
	if err = c.root.Rename(name, "rollback.json"); err != nil {
		return problem("custody", err)
	}
	return c.directory.Sync()
}

func (m Maintenance) readRecord() (Record, error) {
	raw, err := m.Engine.Credentials.read("rollback.json")
	if isAbsent(err) {
		return Record{}, problem("rollback-missing", err)
	}
	if err != nil {
		return Record{}, described("rollback", err, m.Engine.Credentials.pathFor("rollback.json"))
	}
	var r Record
	if json.Unmarshal(raw, &r) != nil {
		return Record{}, problem("rollback", nil)
	}
	return r, nil
}
func (m Maintenance) interrupted(ctx context.Context, namespace string) (*Record, error) {
	r, err := m.readRecord()
	if isAbsent(err) {
		return nil, nil
	}
	if err != nil || r.State == "done" || r.State == "completed" {
		return nil, err
	}
	namespace, err = m.Engine.installationNamespace(namespace)
	if err != nil {
		return nil, err
	}
	if r.Namespace != namespace {
		return nil, described("target", nil, "The upgrade record belongs to a different installation namespace.")
	}
	h, err := m.Engine.Charts.Read(ctx, namespace)
	if err != nil {
		return nil, err
	}
	if r.superseded(h) || sourceUnchanged(r, h.Latest) || r.checkForeign(h) != nil {
		return nil, nil
	}
	if !rollbackLineage(r, h.Latest) {
		return nil, changedRevision(h.Latest.Number, nil)
	}
	return &r, nil
}
func (m Maintenance) repairUpgrade(ctx context.Context, p *UpgradePlan) (Record, error) {
	if err := m.rollback(ctx); err != nil {
		return Record{}, err
	}
	h, err := m.Engine.Charts.Read(ctx, p.record.Namespace)
	if err != nil {
		return Record{}, err
	}
	if h.Latest.Version != p.repair.Source.Version || h.Latest.Status != "deployed" || !rollbackLineage(*p.repair, h.Latest) {
		return Record{}, changedRevision(h.Latest.Number, nil)
	}
	p.Plan.history = h
	p.Plan.request.bindHistory(h)
	p.Plan.request.Recovery = nil
	r := p.record
	r.Source = h.Deployed
	r.Source.Values = h.Choices.Values
	r.TargetRevision = h.Latest.Number + 1
	if !p.options.NoBackup {
		r.BackupPath = m.upgradeBackupPath(r)
	}
	return r, nil
}

func (m *Maintenance) prepareUpgrade(ctx context.Context, r Release, c Choices, o UpgradeOptions) (*Plan, *Record, int, error) {
	repair, err := m.selectUpgradeState(ctx, r, c)
	if err != nil {
		return nil, nil, 0, err
	}
	if err = m.validateUpgrade(r, o); err != nil {
		return nil, nil, 0, err
	}
	p, err := m.Engine.prepare(ctx, r, c, m.Options.Schema)
	if err != nil {
		return nil, nil, 0, err
	}
	fail := func(err error) (*Plan, *Record, int, error) { p.Close(); return nil, nil, 0, err }
	if p.history.Deployed.Number == 0 && p.history.Latest.Status != "superseded" {
		return fail(problem("upgrade-source", nil))
	}
	active := m.ActiveRuns
	if active == nil {
		active = m.Engine.activeRuns
	}
	count, err := active(ctx, p.choices.PipelineNamespace)
	if err != nil {
		return fail(err)
	}

	return p, repair, count, nil
}

func (m Maintenance) beginUpgrade(ctx context.Context, p *UpgradePlan) (record Record, err error) {
	if _, err = m.Engine.revalidate(ctx, p.Plan); err != nil {
		return Record{}, err
	}
	record = p.record
	if p.repair != nil {
		if record, err = m.repairUpgrade(ctx, p); err != nil {
			return Record{}, err
		}
		if _, err = m.Engine.revalidate(ctx, p.Plan); err != nil {
			return Record{}, err
		}
	}
	if err = m.saveRecord(record); err != nil {
		return Record{}, err
	}
	if !p.options.NoBackup {
		if err = m.discardUnrecorded(record.BackupPath); err != nil {
			return Record{}, err
		}
		archive, e := Backup(ctx, BackupConfig{Source: m.Source, Options: record.Options, Recipient: p.Plan.previous.BackupRecipient, Path: record.BackupPath})
		if e != nil {
			return Record{}, e
		}
		archive.StoreSnapshotHint = ""
		record.Archive = &archive
		if err = m.saveRecord(record); err != nil {
			return Record{}, err
		}
	}
	return record, nil
}

func (m Maintenance) validateUpgrade(r Release, o UpgradeOptions) error {
	if m.Engine == nil || !m.Engine.configured() {
		return problem("wiring", nil)
	}
	if !o.NoBackup && m.Source == nil {
		return problem("wiring", nil)
	}
	if !r.Verified() {
		return problem("release", nil)
	}
	if err := m.Options.Validate(); err != nil {
		return err
	}
	if m.Options.Protocol.SchemaIdentity != r.protocol().SchemaIdentity || m.Options.Protocol.AuditDomain != r.protocol().AuditDomain {
		return problem("restore-proof", nil)
	}

	return nil
}
