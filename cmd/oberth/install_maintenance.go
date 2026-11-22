package main

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
)

type installationSnapshot struct {
	remote    *client.Client
	inspector stateport.Backup
}

func (s installationSnapshot) Snapshot(ctx context.Context, options stateport.Options, out io.Writer) (stateport.BackupReport, error) {
	stream, err := s.remote.DatabaseBackup(ctx)
	if err != nil {
		return stateport.BackupReport{}, err
	}
	defer stream.Close()
	return s.inspector.Inspect(ctx, options, io.TeeReader(stream, out))
}

// profileClient reads the named profile files on every connection.
func (s installationSession) profileClient(p installer.Profile) (*client.Client, error) {
	cfg := s.app.config
	cfg.Options = client.Options{URL: p.ServerURL, CAFile: p.CAFile, ClientCert: p.AdminCert, ClientKey: p.AdminKey}
	return client.New(cfg)
}

func (s installationSession) snapshotSource() (installationSnapshot, error) {
	remote, err := s.profileClient(s.profile)
	if err != nil {
		return installationSnapshot{}, err
	}
	dir, err := client.ProfileDir(s.app.base, s.name)
	if err != nil {
		remote.Close()
		return installationSnapshot{}, err
	}
	inspector, err := store.NewBackup(filepath.Join(dir, "backup-inspection.sqlite"))
	if err != nil {
		remote.Close()
		return installationSnapshot{}, err
	}
	return installationSnapshot{remote, inspector}, nil
}

func (s installationSession) maintenance(source installationSnapshot) installer.Maintenance {
	return installer.Maintenance{Engine: s.engine, Source: source,
		Options: s.options,
		ReleaseFiles: func(ctx context.Context, version string) (fs.FS, error) {
			return installationReleaseFiles(ctx, s.request, version)
		}}
}

func installationStateOptions(r installationRequest) (stateport.Options, error) {
	options := stateport.Options{Protocol: stateport.DefaultProtocol(), Schema: store.Schema}
	if path := r.text("config"); path != "" {
		cfg, err := readConfig(path)
		if err != nil {
			return options, err
		}
		options.Protocol = cfg.Protocol
	}
	return options, options.Validate()
}

func (s installationSession) changeInstallation(ctx context.Context, in io.Reader, out io.Writer) error {
	release, err := installationRelease(ctx, s.request)
	if err != nil {
		return err
	}

	source, err := s.snapshotSource()
	if err != nil {
		return err
	}
	defer source.remote.Close()
	maintenance := s.maintenance(source)
	maintenance.LiveState = func(ctx context.Context) (stateport.Options, error) {
		options, err := liveInstallationState(ctx, source.remote)
		if err != nil {
			return options, installationProblem("Cannot read the installation's live schema.", "Run oberth rollback --yes for a recorded same-schema rollback; otherwise restore service, then retry.")
		}
		return options, nil
	}
	plan, err := maintenance.PlanUpgrade(ctx, release, s.choices, installer.UpgradeOptions{NoBackup: s.request.enabled("no-backup")})
	if err != nil {
		return err
	}
	defer plan.Plan.Close()
	return s.applyUpgrade(ctx, plan, upgradeApplication{maintenance: maintenance, in: in, out: out})
}

type upgradeApplication struct {
	maintenance installer.Maintenance
	in          io.Reader
	out         io.Writer
}

func (s installationSession) applyUpgrade(ctx context.Context, plan *installer.UpgradePlan, apply upgradeApplication) error {
	approved, err := installationApproval(ctx, installationReview{request: s.request, plan: plan.Plan, detail: plan.Plan.Detail(), scanner: s.input}, apply.in, apply.out)
	if err != nil || approved == "" {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, installer.ReadinessTimeout+10*time.Minute)
	defer cancel()
	rolled := s.engine.WatchController(ctx)
	result, err := apply.maintenance.Upgrade(ctx, plan, approved, nil)
	if err != nil {
		return err
	}
	// An upgrade issues a new administrator certificate file, so the audit uses the one it left.
	remote, err := s.profileClient(result.Profile)
	if err == nil {
		defer remote.Close()
	}
	s.recordLifecycle(ctx, remote, result.Upgrade, false)
	if err = showInstallation(apply.out, result); err != nil {
		return err
	}
	return controllerRestart(apply.out, rolled())
}

// controllerRestart says, once, that the apply rolled the workflow controller.
func controllerRestart(out io.Writer, rolled bool) error {
	if !rolled {
		return nil
	}
	return writeWrapped(out, installer.ControllerRestart)
}

func (s installationSession) rollback(ctx context.Context, out io.Writer) error {
	source, err := s.snapshotSource()
	if err != nil {
		return err
	}
	defer source.remote.Close()
	maintenance := s.maintenance(source)
	finished := maintenance.RollbackFinished()
	rolled := s.engine.WatchController(ctx)
	if err = maintenance.Rollback(ctx); err != nil {
		return err
	}
	done, err := maintenance.RolledBack(ctx)
	if err != nil {
		return err
	}
	if finished {
		_, err = fmt.Fprintf(out, "Already rolled back to %s, revision %d.\n", done.To, done.Revision)
		return err
	}
	// Rollback keeps the profile's files, so the session's client still names them.
	s.recordLifecycle(ctx, source.remote, done, true)
	if _, err = fmt.Fprintf(out, "Rolled back to %s, revision %d.\n", done.To, done.Revision); err != nil {
		return err
	}
	return controllerRestart(out, rolled())
}

func liveInstallationState(ctx context.Context, remote *client.Client) (stateport.Options, error) {
	var status apicontract.SystemStatus
	if err := remote.Invoke(ctx, "admin_version", apicontract.AdminVersionArgs{}, &status); err != nil {
		return stateport.Options{}, err
	}
	options := stateport.Options{Protocol: status.Protocol, Schema: status.SchemaVersion}
	if options.Validate() != nil {
		return options, installationProblem("Live installation metadata is missing or invalid.", "Use an administrator profile and check oberth system status.")
	}
	return options, nil
}

func (s installationSession) reviewRollback(out io.Writer) error {
	lines, err := (installer.Maintenance{Engine: s.engine}).RollbackReview()
	if err != nil {
		return err
	}
	for _, line := range lines {
		if _, err = fmt.Fprintln(out, safeDisplay(line)); err != nil {
			return err
		}
	}
	if s.request.enabled("dry-run") {
		return nil
	}
	return installationProblem("Rollback requires explicit approval.", "Run oberth rollback --dry-run to review the record, then oberth rollback --yes to apply it.")
}
