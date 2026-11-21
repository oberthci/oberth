package installer

import (
	"context"

	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

// The native query skips undecodable payloads. Compare its decoded history
// with the same query's Secret inventory so damaged revisions cannot disappear.
type releaseSecrets struct {
	driver.Driver
	secrets typedcorev1.SecretInterface
}

func newReleaseSecrets(secrets typedcorev1.SecretInterface) driver.Driver {
	return releaseSecrets{Driver: driver.NewSecrets(secrets), secrets: secrets}
}

func (s releaseSecrets) Query(labels map[string]string) ([]*release.Release, error) {
	inventory := &releaseInventory{SecretInterface: s.secrets}
	records, err := driver.NewSecrets(inventory).Query(labels)
	if err != nil {
		return nil, err
	}
	if len(records) != inventory.count {
		return nil, problem("release-provenance", nil)
	}
	return records, nil
}

type releaseInventory struct {
	typedcorev1.SecretInterface
	count int
}

func (s *releaseInventory) List(ctx context.Context, options metav1.ListOptions) (*corev1.SecretList, error) {
	items, err := s.SecretInterface.List(ctx, options)
	if err == nil {
		s.count = len(items.Items)
	}
	return items, err
}
