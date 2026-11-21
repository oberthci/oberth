package installer

import (
	"context"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (c *ConfigDir) checkTarget(context, uid, namespace string) error {
	p, err := c.profile()
	if isAbsent(err) {
		return nil
	}
	if err != nil {
		return described("custody", err, c.pathFor("installation.json"))
	}
	if p.Namespace != namespace {
		return described("profile-namespace", nil, "Profile directory: "+c.path)
	}
	if p.Context != context || p.ClusterUID != uid {
		return described("profile-cluster", nil, "Profile directory: "+c.path)
	}
	return nil
}
func secrets(i Identities) []*corev1.Secret { return []*corev1.Secret{i.CA, i.Host, i.Forge, i.Store} }
func (e *Engine) persistIdentities(ctx context.Context, p *Plan) (Identities, error) {
	client := e.Cluster.Client.CoreV1().Secrets(p.choices.Namespace)
	for _, s := range secrets(p.identities) {
		live, err := client.Get(ctx, s.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = client.Create(ctx, s.DeepCopy(), metav1.CreateOptions{})
			if err != nil && !apierrors.IsAlreadyExists(err) {
				return Identities{}, err
			}
			live, err = client.Get(ctx, s.Name, metav1.GetOptions{})
		}
		if err != nil || !reflect.DeepEqual(live.Data, s.Data) {
			return Identities{}, described("target", err, "Installation Secret changed after review: "+s.Namespace+"/"+s.Name)
		}
	}
	return Identities{
		CA:    p.identities.CA.DeepCopy(),
		Host:  p.identities.Host.DeepCopy(),
		Forge: p.identities.Forge.DeepCopy(),
		Store: p.identities.Store.DeepCopy(),
	}, nil
}

func (e *Engine) installationNamespace(requested string) (string, error) {
	if requested != "" {
		return requested, nil
	}
	profile, err := e.Credentials.profile()
	if err != nil && !isAbsent(err) {
		return "", problem("custody", err)
	}
	if profile.Namespace != "" {
		return profile.Namespace, nil
	}
	return Defaults().Namespace, nil
}

func (e *Engine) checkInstallationTarget(ctx context.Context, c Choices, uid string) error {
	old, err := e.Credentials.profile()
	if isAbsent(err) {
		return nil
	}
	if err != nil {
		return problem("custody", err)
	}
	if err = e.Credentials.checkTarget(c.Context, uid, old.Namespace); err != nil {
		return err
	}
	if old.Namespace == c.Namespace {
		return nil
	}
	history, err := e.Charts.Read(ctx, old.Namespace)
	if err != nil {
		return chartError("cluster", err)
	}
	if hasRevisions(history) {
		return described("profile-namespace", nil, "Profile directory: "+e.Credentials.path)
	}
	return nil
}
