package installer

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Result exposes public installation facts and paths, never credential bytes.
type Result struct {
	Upgrade            UpgradeCompletion `json:"upgrade,omitzero"`
	Profile            Profile           `json:"profile"`
	SSHHostFingerprint string            `json:"ssh_host_fingerprint"`
	DeployPublicKey    string            `json:"deploy_public_key"`
	Next               string            `json:"next"`
	StoreSnapshotHint  string            `json:"store_snapshot_hint"`
	BackupPath         string            `json:"backup_path,omitempty"`
	BackupReminder     string            `json:"backup_reminder"`
}

// UpgradeCompletion contains facts from the completed local recovery record.
type UpgradeCompletion struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Revision int    `json:"revision"`
}

// Progress receives short engine-owned reasons. Returning an error interrupts safely.
type Progress func(Phase) error

func emit(progress Progress, phase Phase) error {
	if progress == nil {
		return nil
	}
	return progress(phase)
}

// Apply requires the reviewed digest and revalidates mutable facts before writes.
func (e *Engine) Apply(ctx context.Context, p *Plan, approved string, progress Progress) (Result, error) {
	if p == nil || approved == "" || approved != p.digest {
		return Result{}, problem("review", nil)
	}
	if !e.configured() {
		return Result{}, problem("wiring", nil)
	}
	unlock, err := e.Credentials.lock()
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	return e.apply(ctx, p, approved, progress)
}
func (e *Engine) apply(ctx context.Context, p *Plan, approved string, progress Progress) (Result, error) {
	if err := emit(progress, Checking); err != nil {
		return Result{}, err
	}
	if err := emit(progress, ReleaseChecked); err != nil {
		return Result{}, err
	}
	absent, err := e.revalidate(ctx, p)
	if err != nil {
		return Result{}, err
	}
	if err := e.beginInstallation(ctx, p, absent); err != nil {
		return Result{}, err
	}
	profile, err := e.createCredentials(ctx, p, progress)
	if err != nil {
		return Result{}, err
	}
	if err = e.finish(ctx, p, profile, progress); err != nil {
		return Result{}, err
	}
	reminder := "Copy " + profile.BackupKey + " off this machine."
	if profile.RecoveryKey != "" {
		reminder += "\nCopy " + profile.RecoveryKey + " off this machine."
	}
	return Result{
		StoreSnapshotHint:  StoreSnapshotHint,
		Profile:            profile,
		SSHHostFingerprint: p.identities.hostFingerprint(),
		DeployPublicKey:    string(p.identities.Forge.Data["public"]),
		Next:               "Add the deploy key to your forge, then run oberth repo add <forge URL>.",
		BackupReminder:     reminder,
	}, nil
}
func (e *Engine) revalidate(ctx context.Context, p *Plan) (bool, error) {
	uid, err := e.Cluster.UID(ctx)
	if err != nil {
		return false, problem("cluster", err)
	}
	if err = e.checkInstallationTarget(ctx, p.choices, uid); err != nil {
		return false, err
	}
	h, err := e.Charts.Read(ctx, p.choices.Namespace)
	if err != nil {
		return false, chartError("cluster", err)
	}
	if err = e.checkUpdateOwner(ctx, p.choices.Namespace, h); err != nil {
		return false, err
	}
	if uid != p.uid {
		return false, described("target", nil, "The selected cluster identity changed after review.")
	}
	if !reflect.DeepEqual(h, p.history) {
		if err = p.request.checkBaseline(h); err != nil {
			return false, err
		}
		return false, described("target", nil, fmt.Sprintf("Release history or saved choices changed after review; latest revision %d.", h.Latest.Number))
	}
	absent, err := e.recheckSharedCluster(ctx, p)
	if err != nil {
		return false, err
	}
	if err = p.checkRelease(); err != nil {
		return false, err
	}
	if err = e.readIdentities(ctx, p.choices.Namespace, p.history.Deployed.Number > 0); err != nil {
		return false, err
	}
	return absent, chartError("chart", e.Charts.Preview(ctx, p.request))
}
func (e *Engine) createCredentials(ctx context.Context, p *Plan, progress Progress) (Profile, error) {
	if err := emit(progress, Creating); err != nil {
		return Profile{}, err
	}
	ids, err := e.persistIdentities(ctx, p)
	if err != nil {
		return Profile{}, problem("identity", err)
	}
	defer ids.clear()
	if err = emit(progress, Saving); err != nil {
		return Profile{}, err
	}
	profile, err := e.saveProfile(p, ids, p.admin)
	if err != nil {
		return Profile{}, problem("custody", err)
	}
	if err = e.bootstrap(ctx, p.choices.Namespace, p.admin); err != nil {
		return Profile{}, err
	}
	profile.BootstrapPending = false
	if err = e.Credentials.saveProfile(profile, p.admin.id); err != nil {
		return Profile{}, err
	}
	helper, repointed := e.repointGit(ctx, profile)
	if !repointed {
		return profile, nil
	}
	return profile, e.Credentials.prune(profile, helper)
}

// repointGit runs before the earlier credential files go, so the helper never names a removed key.
func (e *Engine) repointGit(ctx context.Context, p Profile) ([]string, bool) {
	if e.GitHelper == nil {
		return nil, true
	}
	return e.GitHelper(ctx, p)
}
func (e *Engine) finish(ctx context.Context, p *Plan, profile Profile, progress Progress) error {
	rolled := e.watchController(ctx, p.choices.Namespace)
	if err := e.applyChart(ctx, p, progress); err != nil {
		return err
	}
	limit := e.waitLimit()
	reason := Phase(fmt.Sprintf("%s (up to %.0f s)", Waiting, math.Ceil(limit.Seconds())))
	if err := emit(progress, reason); err != nil {
		return err
	}
	waiting, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	if err := e.waitReady(waiting, profile); err != nil {
		return err
	}
	// A changed controller is ready only when no Pod of the earlier one serves.
	if rolled() {
		if err := e.waitController(waiting, p.choices.Namespace); err != nil {
			return err
		}
	}
	if err := emit(progress, Endpoint); err != nil {
		return err
	}
	return emit(progress, Installed)
}
func (e *Engine) applyChart(ctx context.Context, p *Plan, progress Progress) error {
	chart, cancel := context.WithTimeout(ctx, e.chartLimit())
	defer cancel()
	if err := emit(progress, Installing); err != nil {
		return err
	}
	if err := e.Charts.Apply(chart, p.request); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(chart.Err(), context.DeadlineExceeded) {
			return problem("chart-timeout", err)
		}
		return chartError("chart", err)
	}
	if err := e.startStore(chart, p, progress); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(chart.Err(), context.DeadlineExceeded) {
			return chartError("store-timeout", err)
		}
		return err
	}
	return nil
}
func (e *Engine) chartLimit() time.Duration {
	if e.ChartTimeout > 0 {
		return e.ChartTimeout
	}
	return 5 * time.Minute
}
func (e *Engine) waitLimit() time.Duration {
	if e.Timeout > 0 {
		return e.Timeout
	}
	return ReadinessTimeout
}

func (e *Engine) bootstrap(ctx context.Context, ns string, a admin) error {
	secrets := e.Cluster.Client.CoreV1().Secrets(ns)
	s, err := secrets.Get(ctx, "oberth-admin", metav1.GetOptions{})
	data := map[string][]byte{"admin.pub": a.public, "push.pub": a.public}
	if apierrors.IsNotFound(err) {
		_, err = secrets.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "oberth-admin", Namespace: ns}, Data: data}, metav1.CreateOptions{})
	} else if err == nil {
		if reflect.DeepEqual(s.Data, data) {
			return nil
		}
		return installationOwner(ns)
	}
	if err != nil {
		return problem("identity", err)
	}
	return nil
}
