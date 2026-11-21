package installer

import (
	"context"
	"errors"
	"path/filepath"

	"golang.org/x/crypto/ssh"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A release or an already-published administrator requires the original private key.
func (e *Engine) checkUpdateOwner(ctx context.Context, namespace string, h History) error {
	secret, err := e.Cluster.Client.CoreV1().Secrets(namespace).Get(ctx, "oberth-admin", meta.GetOptions{})
	if apierrors.IsNotFound(err) && !hasRevisions(h) {
		return nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return problem("cluster", err)
	}
	if err != nil {
		return described("administrator-missing", nil, "Secret: "+namespace+"/oberth-admin (missing)")
	}
	public, err := e.savedAdminPublic()
	if err != nil || !adminMatches(admin{public: public}, secret.Data) {
		if !hasRevisions(h) {
			return described("administrator-retained", nil, "Secret: "+namespace+"/oberth-admin (retained after release removal)")
		}
		return installationOwner(namespace)
	}
	return nil
}

func (e *Engine) savedAdminPublic() ([]byte, error) {
	profile, err := e.Credentials.profile()
	if err != nil || profile.AdminKey == "" {
		return nil, problem("profile-key", err)
	}
	raw, err := e.Credentials.read(filepath.Base(profile.AdminKey))
	if err != nil {
		return nil, err
	}
	defer clear(raw)
	_, key, err := adminKey(e, raw)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	public, err := ssh.NewPublicKey(key.Public())
	if err != nil {
		return nil, err
	}
	return ssh.MarshalAuthorizedKey(public), nil
}

func installationOwner(namespace string) error {
	return described("installation-owner", nil, "Installation: "+namespace+"/oberth")
}

func (e *Engine) checkReviewedOwner(ctx context.Context, p *Plan) error {
	_, err := e.ownedHistory(ctx, p.choices.Namespace)
	return err
}

func (e *Engine) ownedHistory(ctx context.Context, namespace string) (History, error) {
	history, err := e.Charts.Read(ctx, namespace)
	if err != nil {
		var refusal *Problem
		if errors.As(err, &refusal) && refusal.Code == "release-unowned" {
			if _, keyErr := e.savedAdminPublic(); keyErr != nil {
				return History{}, described("namespace-installation", err, "Namespace: "+namespace)
			}
		}
		return History{}, chartError("cluster", err)
	}
	return history, e.checkUpdateOwner(ctx, namespace, history)
}
