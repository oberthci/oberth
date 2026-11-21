package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Rollback verifies the earlier release before applying its complete chart.
func (m Maintenance) Rollback(ctx context.Context) error {
	if m.Engine == nil || !m.Engine.configured() {
		return problem("wiring", nil)
	}
	ctx, cancel := context.WithTimeout(ctx, m.Engine.chartLimit()+m.Engine.waitLimit()+2*time.Minute)
	defer cancel()
	unlock, err := m.Engine.Credentials.lock()
	if err != nil {
		return err
	}
	defer unlock()
	return m.rollback(ctx)
}
func (m Maintenance) rollback(ctx context.Context) error {
	r, err := m.readRecord()
	if err != nil {
		return err
	}
	if r.Options.Schema != r.TargetSchema {
		return m.schemaRecovery("rollback-schema", nil, r)
	}
	uid, err := m.Engine.Cluster.UID(ctx)
	if err != nil {
		return problem("cluster", err)
	}
	if r.Context != m.Engine.Cluster.Context || r.ClusterUID != uid {
		return described("target", nil, "The rollback record belongs to a different cluster or context.")
	}
	history, err := m.Engine.Charts.Read(ctx, r.Namespace)
	if err != nil {
		return err
	}
	if r.superseded(history) {
		return problem("rollback-superseded", nil)
	}
	if err = r.checkForeign(history); err != nil {
		return err
	}
	if r.State == "done" {
		return nil
	}
	if sourceUnchanged(r, history.Latest) {
		return m.finishRollback(ctx, r, false)
	}
	if !rollbackLineage(r, history.Latest) {
		return changedRevision(history.Latest.Number, nil)
	}
	return m.applyRollback(ctx, r)
}
func (m Maintenance) applyRollback(ctx context.Context, r Record) error {
	plan, err := m.rollbackPlan(ctx, r)
	if err != nil {
		return err
	}
	defer plan.Close()
	rolled := m.Engine.watchController(ctx, r.Namespace)
	if err = m.revertChart(ctx, r, plan); err != nil {
		return err
	}
	return m.finishRollback(ctx, r, rolled())
}
func (m Maintenance) rollbackPlan(ctx context.Context, r Record) (*Plan, error) {
	if m.ReleaseFiles == nil {
		return nil, problem("wiring", nil)
	}
	files, err := m.ReleaseFiles(ctx, r.Source.Version)
	if err != nil {
		return nil, problem("release", err)
	}
	earlier, err := LoadRelease(ctx, files, r.Source.Version)
	if err != nil {
		return nil, rollbackReleaseProblem(err, r.Source.Version)
	}
	if earlier.schema() != r.Options.Schema || earlier.protocol().SchemaIdentity != r.Options.Protocol.SchemaIdentity || earlier.protocol().AuditDomain != r.Options.Protocol.AuditDomain {
		return nil, problem("restore-proof", nil)
	}
	c := deployedChoices(Choices{Context: r.Context, Namespace: r.Namespace, Values: operatorValues(merge(earlier.chart.Values, r.Source.Values))}, r.Source.Values)
	// The source pins come from the recorded source revision, not from the
	// revision being rolled back.
	c.ClearSourcePins = true
	if c.SourcePins, err = savedSourcePins(r.Source.Values); err != nil {
		return nil, described("source-pins", err, "Saved policy: "+err.Error())
	}
	p, err := m.Engine.prepare(ctx, earlier, c, earlier.schema())
	if err != nil {
		return nil, err
	}
	p.request.Recovery = nil
	p.request.RollbackID = r.ID()
	if err = m.validateRollback(ctx, r, p); err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}
func (m Maintenance) validateRollback(ctx context.Context, r Record, p *Plan) error {
	if !rollbackLineage(r, p.history.Latest) {
		return changedRevision(p.history.Latest.Number, nil)
	}
	_, err := m.Engine.revalidate(ctx, p)
	return err
}
func (m Maintenance) revertChart(ctx context.Context, r Record, plan *Plan) error {
	latest := plan.history.Latest
	if latest.RollbackID == r.ID() && latest.Version == r.Source.Version && latest.Status == "deployed" {
		return nil
	}
	request := plan.request
	chart, cancel := context.WithTimeout(ctx, m.Engine.chartLimit())
	defer cancel()
	return m.Engine.Charts.Apply(chart, request)
}

// finishRollback waits for the restored server and, when the rollback changed
// the controller, for the restored controller, within one readiness deadline.
func (m Maintenance) finishRollback(ctx context.Context, r Record, rolled bool) error {
	profile, err := m.Engine.Credentials.profile()
	if err != nil {
		return err
	}
	profile.ServerVersion = r.Source.Version
	waiting, cancel := context.WithTimeout(ctx, m.Engine.waitLimit())
	defer cancel()
	if err = m.Engine.waitReady(waiting, profile); err == nil && rolled {
		err = m.Engine.waitController(waiting, r.Namespace)
	}
	if err != nil {
		return described("upgrade-failed", err, "Retry: oberth rollback")
	}
	raw, _ := json.Marshal(r)
	if err = m.Engine.Credentials.saveProfile(profile, publicDigest(raw)); err != nil {
		return err
	}
	// Rollback keeps every credential file, so a helper that cannot be repointed loses nothing.
	_, _ = m.Engine.repointGit(ctx, profile)
	r.State = "done"
	return m.saveRecord(r)
}

// RollbackFinished reports whether the saved record is already rolled back;
// a missing or unreadable record is left to Rollback to report.
func (m Maintenance) RollbackFinished() bool {
	r, err := m.readRecord()
	return err == nil && r.State == "done"
}

// RolledBack names the release and revision a completed rollback left deployed.
func (m Maintenance) RolledBack(ctx context.Context) (UpgradeCompletion, error) {
	r, err := m.readRecord()
	if err != nil {
		return UpgradeCompletion{}, err
	}
	h, err := m.Engine.Charts.Read(ctx, r.Namespace)
	if err != nil {
		return UpgradeCompletion{}, err
	}
	return UpgradeCompletion{From: r.TargetVersion, To: h.Latest.Version, Revision: h.Latest.Number}, nil
}

func rollbackLineage(r Record, latest Revision) bool {
	if latest.Number == r.TargetRevision {
		return latest.Version == r.TargetVersion && latest.Installer()
	}
	return latest.Number > r.TargetRevision && latest.Version == r.Source.Version && latest.Installer() && latest.RollbackID == r.ID()
}

func sourceUnchanged(r Record, latest Revision) bool {
	return latest.Number == r.Source.Number && latest.Version == r.Source.Version && latest.Status == "deployed" && latest.Kind == r.Source.Kind
}

func (r Record) superseded(h History) bool {
	return h.Choices.Number > r.TargetRevision && !rollbackLineage(r, h.Latest)
}
func (r Record) checkForeign(h History) error {
	for _, number := range h.Foreign {
		if number > r.TargetRevision {
			return described("foreign-rollback", nil, fmt.Sprintf("Latest revision %d: %s (%s).", h.Latest.Number, h.Latest.Version, h.Latest.Status))
		}
	}
	return nil
}

func rollbackReleaseProblem(err error, version string) error {
	var refusal *Problem
	if !errors.As(err, &refusal) {
		return described("release", err, "Required rollback version: "+version)
	}
	public := *refusal
	public.Detail = strings.TrimSpace(public.Detail + "\nRequired rollback version: " + version)
	return &public
}

// RollbackReview shows the saved transition without contacting or changing the installation.
func (m Maintenance) RollbackReview() ([]string, error) {
	r, err := m.readRecord()
	if err != nil {
		return nil, err
	}
	return []string{
		"Rollback record: " + m.Engine.Credentials.pathFor("rollback.json"),
		fmt.Sprintf("Source: %s; revision %d; schema %d", r.Source.Version, r.Source.Number, r.Options.Schema),
		fmt.Sprintf("Target: %s; revision %d; schema %d", r.TargetVersion, r.TargetRevision, r.TargetSchema),
		"State: " + r.State,
		"Backup: " + r.BackupPath,
	}, nil
}
