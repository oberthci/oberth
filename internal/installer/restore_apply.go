package installer

import (
	"context"
	"io"
	"time"

	"github.com/oberthci/oberth/internal/stateport"
)

// RestoreConfig requires separate private recovery material and explicit approval.
type RestoreConfig struct {
	Archive       RestoreReviewConfig
	Release       Release
	Choices       Choices
	Administrator []byte
	Unseal        []byte
	Yes           bool
}

// Restore installs into an unused namespace on a fresh cluster. It never adopts
// an existing installation, database volume or partially completed restore.
func (e *Engine) Restore(ctx context.Context, c RestoreConfig) (Result, error) {
	if !c.Yes || !e.configured() || len(c.Administrator) == 0 || c.Choices.URL == "" {
		return Result{}, restoreApplyProblem()
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	files, review, err := stageRestore(ctx, c.Archive)
	if err != nil {
		return Result{}, err
	}
	defer files.close()
	if files.info.BundledStore && (!review.StoreIncluded || len(c.Unseal) == 0) {
		return Result{}, restoreApplyProblem()
	}
	unlock, err := e.Credentials.lock()
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	p, err := e.prepareRestore(ctx, c, files)
	if err != nil {
		return Result{}, err
	}
	defer p.Close()
	return e.applyRestore(ctx, p, files, c.Unseal)
}

func (e *Engine) applyRestore(ctx context.Context, p *Plan, files *restoreFiles, unseal []byte) (Result, error) {
	var err error
	if err = e.claimRestore(ctx, p, files); err != nil {
		return Result{}, err
	}
	profile, err := e.saveProfile(ctx, p, p.identities, p.admin)
	if err != nil {
		return Result{}, restoreApplyProblem()
	}
	if err = e.Charts.Apply(ctx, p.request); err != nil {
		return Result{}, restoreApplyProblem()
	}
	if err = e.restoreStore(ctx, p, files, unseal); err != nil {
		return Result{}, err
	}
	if err = e.restoreDatabase(ctx, p, files, files.report); err != nil {
		return Result{}, err
	}
	return e.finishRestore(ctx, p, profile)
}

func (e *Engine) restoreDatabase(ctx context.Context, p *Plan, files *restoreFiles, expected stateport.BackupReport) error {
	in := files.database
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return problem("restore-database", nil)
	}
	transfer := e.RestoreDatabase
	if transfer == nil {
		transfer = e.attachRestoreDatabase
	}
	report, err := transfer(ctx, p.choices.Namespace, files.options, in)
	if err != nil || report != expected {
		return problem("restore-database", nil)
	}
	return nil
}

func (e *Engine) finishRestore(ctx context.Context, p *Plan, profile Profile) (Result, error) {
	history, err := e.ownedHistory(ctx, profile.Namespace)
	if err != nil {
		return Result{}, err
	}
	p.request.bindHistory(history)
	p.request.Values = merge(p.request.Values, map[string]any{"restorePending": false})
	if err = e.Charts.Apply(ctx, p.request); err != nil {
		return Result{}, restoreApplyProblem()
	}
	if err = e.waitReady(ctx, profile, ""); err != nil {
		return Result{}, err
	}
	profile.Ready, profile.BootstrapPending = true, false
	if err = e.Credentials.saveProfile(profile, p.admin.id); err != nil {
		return Result{}, restoreApplyProblem()
	}
	_, _ = e.repointGit(ctx, profile)
	return Result{Profile: profile, SSHHostFingerprint: p.identities.hostFingerprint(),
		DeployPublicKey:   string(p.identities.Forge.Data["public"]),
		Next:              "Installation restored. Check repository access and workflow schedules before resuming work.",
		StoreSnapshotHint: "Restored the database, installation Secrets, values and included store data."}, nil
}

func restoreApplyProblem() error {
	return problem("restore-input", nil)
}

// DatabaseRestore streams to the stopped installation's one-use restore Pod.
type DatabaseRestore func(context.Context, string, stateport.Options, io.Reader) (stateport.BackupReport, error)
