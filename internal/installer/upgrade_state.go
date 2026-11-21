package installer

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/stateport"
)

func (m Maintenance) validateRepair(ctx context.Context, target Release, r Record) error {
	if !target.Verified() {
		return problem("release", nil)
	}
	if r.State != "pending" || r.Options.Validate() != nil {
		return problem("rollback", nil)
	}
	if err := m.validateRecordedUpgrade(ctx, r); err != nil {
		return err
	}
	if r.TargetVersion != target.Version() || r.TargetSchema != target.schema() {
		return described("repair-release", nil, "Recorded release: "+r.TargetVersion)
	}
	return nil
}

func (m Maintenance) validateRecordedUpgrade(ctx context.Context, r Record) error {
	if (r.State != "pending" && r.State != "completed") || r.Options.Validate() != nil {
		return problem("rollback", nil)
	}
	uid, err := m.Engine.Cluster.UID(ctx)
	if err != nil {
		return problem("cluster", err)
	}
	if r.Context != m.Engine.Cluster.Context || r.ClusterUID != uid {
		return described("target", nil, "The upgrade record belongs to a different cluster or context.")
	}
	return nil
}

// A successful version-bound readiness check permits completing a timed-out upgrade.
func (m Maintenance) confirmReadyTarget(ctx context.Context, r Record) (stateport.Options, error) {
	if err := m.validateRecordedUpgrade(ctx, r); err != nil {
		return stateport.Options{}, err
	}
	profile, err := m.Engine.Credentials.profile()
	if err != nil {
		return stateport.Options{}, err
	}
	profile.ServerVersion = r.TargetVersion
	if err = m.Engine.CheckEndpoint(ctx, profile); err != nil {
		var refusal *Problem
		if errors.As(err, &refusal) && refusal.Code == "ready-version" {
			recovery := m.completionRefusal("ready-version", err, r)
			recovery.Detail += "\n" + refusal.Detail
			return stateport.Options{}, recovery
		}
		return stateport.Options{}, m.completionRefusal("repair-schema", err, r)
	}
	options := r.Options
	options.Schema = r.TargetSchema
	if m.LiveState != nil {
		options, err = m.LiveState(ctx)
		if err != nil {
			return options, m.completionRefusal("repair-schema", err, r)
		}
	}
	if options.Validate() != nil || options.Schema != r.TargetSchema || options.Protocol != r.Options.Protocol {
		return options, m.completionRefusal("repair-schema", nil, r)
	}
	return options, nil
}

// A same-schema target that stops being ready after review is reviewed again;
// a cross-schema one has no repair to fall back on.
func (m Maintenance) completionRefusal(code string, cause error, r Record) *Problem {
	if r.Options.Schema == r.TargetSchema {
		return NewProblem("target", cause, "The recorded release is no longer ready.")
	}
	return m.schemaRecovery(code, cause, r)
}

// servesTarget reports whether an interrupted upgrade left its target revision
// deployed and ready, so completing the record replaces a rollback and reapply.
func (m Maintenance) servesTarget(ctx context.Context, r Record) bool {
	h, err := m.Engine.Charts.Read(ctx, r.Namespace)
	if err != nil || h.Latest.Number != r.TargetRevision || h.Latest.Version != r.TargetVersion || h.Latest.Status != "deployed" {
		return false
	}
	profile, err := m.Engine.Credentials.profile()
	if err != nil {
		return false
	}
	profile.ServerVersion = r.TargetVersion
	probe, cancel := context.WithTimeout(ctx, targetProbe)
	defer cancel()
	return m.Engine.CheckEndpoint(probe, profile) == nil
}

// targetProbe bounds the review's readiness check of a server that may be down.
const targetProbe = 10 * time.Second

func (m Maintenance) schemaRecovery(code string, cause error, r Record) *Problem {
	detail := "Recovery is manual in this version."
	if r.BackupPath != "" {
		detail += "\nEncrypted pre-upgrade backup: " + r.BackupPath
	} else {
		detail += "\nNo pre-upgrade backup was requested."
	}
	detail += "\nBackup key: " + m.Engine.Credentials.pathFor("backup.agekey")
	detail += "\nRollback record: " + m.Engine.Credentials.pathFor("rollback.json")
	if cause != nil {
		detail += "\n" + upgradeCause(cause)
	}
	return NewProblem(code, cause, detail)
}

func upgradeCause(err error) string {
	var refusal *Problem
	if !errors.As(err, &refusal) {
		return err.Error()
	}
	// A store refusal needs an operator action, such as oberth unseal, that the upgrade remedy does not name.
	if strings.HasPrefix(refusal.Code, "store-") {
		return refusal.Reason + "\n" + refusal.Remedy
	}
	return refusal.Reason
}

func (m *Maintenance) selectUpgradeState(ctx context.Context, target Release, c Choices) (*Record, error) {
	if m.Engine == nil || !m.Engine.configured() {
		return nil, problem("wiring", nil)
	}
	repair, err := m.interrupted(ctx, c.Namespace)
	if err != nil {
		return nil, err
	}
	if repair != nil && repair.Options.Schema != repair.TargetSchema {
		return m.selectCompletedUpgrade(ctx, target, *repair)
	}
	if repair != nil && m.servesTarget(ctx, *repair) {
		// A same-schema completion that cannot be verified is repaired, like a target that is not ready.
		if completed, err := m.selectCompletedUpgrade(ctx, target, *repair); err == nil {
			return completed, nil
		}
	}

	if m.LiveState == nil {
		return repair, nil
	}
	if repair == nil {
		m.Options, err = m.LiveState(ctx)
		return nil, err
	}
	if err = m.validateRepair(ctx, target, *repair); err != nil {
		return nil, err
	}
	m.Options = repair.Options
	return repair, nil
}

func (m Maintenance) completeUpgrade(ctx context.Context, p *UpgradePlan) (Result, error) {
	if _, err := m.Engine.revalidate(ctx, p.Plan); err != nil {
		return Result{}, err
	}
	if _, err := m.confirmReadyTarget(ctx, *p.complete); err != nil {
		return Result{}, err
	}
	if err := m.Engine.storeReady(ctx, p.Plan); err != nil {
		return Result{}, err
	}
	record := *p.complete
	record.State = "completed"
	if err := m.saveRecord(record); err != nil {
		return Result{}, err
	}
	// Completing the record changes nothing, so it writes, names and prunes no backup.
	return Result{Profile: p.Plan.previous, Upgrade: UpgradeCompletion{From: record.Source.Version, To: record.TargetVersion, Revision: record.TargetRevision}, Next: "Upgrade completed; the recorded release is ready."}, nil
}

func (m *Maintenance) selectCompletedUpgrade(ctx context.Context, target Release, r Record) (*Record, error) {
	if !target.Verified() {
		return nil, problem("release", nil)
	}
	if target.Version() == r.TargetVersion && target.schema() != r.TargetSchema {
		return nil, problem("repair-release", nil)
	}
	options, err := m.confirmReadyTarget(ctx, r)
	if err != nil {
		return nil, err
	}
	m.Options = options
	r.State = "completed"
	return &r, nil
}

// upgradeRoute returns the completion plan of a completed record, or the record
// to repair, or neither for an ordinary upgrade. Completion and an ordinary
// upgrade both hold only what the apply's store step would accept.
func (m Maintenance) upgradeRoute(ctx context.Context, p *Plan, r Release, repair *Record) (*UpgradePlan, *Record, error) {
	if repair != nil && repair.State == "completed" {
		if repair.TargetVersion == r.Version() && p.history.Latest.Status == "deployed" {
			if err := m.Engine.storeReady(ctx, p); err != nil {
				return nil, nil, err
			}
			return completedUpgradePlan(p, *repair), nil, nil
		}
		repair = nil
	}
	if repair == nil {
		return nil, nil, m.Engine.upgradeStore(ctx, p)
	}
	return nil, repair, nil
}

func completedUpgradePlan(p *Plan, r Record) *UpgradePlan {
	p.review = review(p, r.Source.Version+" -> "+r.TargetVersion)
	p.review = append(p.review, "Recovery: complete the ready upgrade; no chart or database changes.")
	p.rehash()
	return &UpgradePlan{Plan: p, record: r, complete: &r, options: UpgradeOptions{NoBackup: true}}
}
