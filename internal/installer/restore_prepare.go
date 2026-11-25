package installer

import (
	"context"
	"os"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (e *Engine) prepareRestore(ctx context.Context, c RestoreConfig, f *restoreFiles) (*Plan, error) {
	info := f.info
	if c.Release.chart == nil || c.Release.schema() != c.Archive.Options.Schema || c.Release.protocol() != c.Archive.Options.Protocol {
		return nil, restoreApplyProblem()
	}
	uid, err := e.freshRestoreTarget(ctx, info)
	if err != nil {
		return nil, err
	}
	c.Choices.Namespace, c.Choices.PipelineNamespace = info.Namespace, info.PipelineNamespace
	c.Choices = deployedChoices(c.Choices, info.Values)
	c.Choices.Values = operatorValues(info.Values)
	c.Choices.SourcePins, err = savedSourcePins(info.Values)
	if err != nil {
		return nil, restoreApplyProblem()
	}
	p, err := e.prepare(ctx, c.Release, c.Choices, c.Archive.Options.Schema)
	if err != nil {
		return nil, err
	}
	if p.uid != uid {
		p.Close()
		return nil, restoreApplyProblem()
	}
	if err = e.restoreIdentities(p, f, c.Administrator); err != nil {
		p.Close()
		return nil, err
	}
	p.request.Values = merge(p.request.Values, map[string]any{"restorePending": true,
		"restoreMaxBytes":      c.Archive.Options.MaxBytes,
		"server":               map[string]any{"runtime": map[string]any{"source": map[string]any{"ca_pem": string(p.identities.CA.Data["tls.crt"])}}},
		"adminPublicKeySHA256": publicDigest(p.admin.public)})
	if err = e.Charts.Preview(ctx, p.request); err != nil {
		p.Close()
		return nil, restoreApplyProblem()
	}
	p.rehash()
	return p, nil
}

func (e *Engine) freshRestoreTarget(ctx context.Context, info installationBackupInfo) (string, error) {
	uid, err := e.Cluster.UID(ctx)
	if err != nil || uid == info.ClusterUID {
		return "", restoreApplyProblem()
	}
	if _, err = e.Credentials.profile(); !os.IsNotExist(err) {
		return "", problem("restore-occupied", nil)
	}
	if _, err = e.Cluster.Client.CoreV1().Namespaces().Get(ctx, info.Namespace, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		return "", problem("restore-occupied", nil)
	}
	return uid, nil
}

func (e *Engine) restoreIdentities(p *Plan, f *restoreFiles, administrator []byte) error {
	p.identities.clear()
	clear(p.admin.key)
	items := map[string]*corev1.Secret{}
	for _, saved := range f.secrets {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: saved.Name, Namespace: f.info.Namespace}, Type: saved.Type, Data: saved.Data}
		items[saved.Name] = secret.DeepCopy()
	}
	p.identities = Identities{CA: items["oberth-ca"], Host: items["oberth-ssh-host"], Forge: items["oberth-forge-key"], Store: items["oberth-store-tls"]}
	for _, secret := range secrets(p.identities) {
		if !validSecret(secret) {
			return restoreApplyProblem()
		}
	}
	if err := e.verifyBundle(p.identities, p.choices.Namespace); err != nil {
		return restoreApplyProblem()
	}
	var err error
	p.admin, err = e.administrator(p.identities, append([]byte(nil), administrator...))
	if err != nil || !adminMatches(p.admin, items["oberth-admin"].Data) {
		return restoreApplyProblem()
	}
	return nil
}

func (e *Engine) claimRestore(ctx context.Context, p *Plan, files *restoreFiles) error {
	absent, err := e.revalidate(ctx, p)
	if err != nil || !absent || hasRevisions(p.history) {
		return restoreApplyProblem()
	}
	if err = e.beginInstallation(ctx, p, true); err != nil {
		return err
	}
	for _, saved := range files.secrets {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: saved.Name, Namespace: p.choices.Namespace}, Type: saved.Type, Data: saved.Data}
		if _, err = e.Cluster.Client.CoreV1().Secrets(p.choices.Namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
			return restoreApplyProblem()
		}
	}
	return nil
}
