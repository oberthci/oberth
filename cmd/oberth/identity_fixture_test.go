package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/oberthci/oberth/internal/app"
)

type fixtureIdentityStore struct {
	client    func() (kubernetes.Interface, error)
	namespace string
}

func (s fixtureIdentityStore) Load(ctx context.Context, name string) (map[string][]byte, error) {
	c, err := s.client()
	if err != nil {
		return nil, err
	}
	value, err := c.CoreV1().Secrets(s.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return value.Data, nil
}
func (s fixtureIdentityStore) Save(ctx context.Context, name string, values map[string][]byte) error {
	c, err := s.client()
	if err != nil {
		return err
	}
	old, err := s.Load(ctx, name)
	if err != nil {
		return err
	}
	if old == nil {
		old = map[string][]byte{}
	}
	for k, v := range values {
		if len(old[k]) > 0 && !bytes.Equal(old[k], v) && k != "known_hosts" {
			return errors.New("identity already exists; refusing to overwrite")
		}
		old[k] = v
	}
	body, err := json.Marshal(&corev1.Secret{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}, ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.namespace}, Data: old})
	if err != nil {
		return err
	}
	force := true
	_, err = c.CoreV1().Secrets(s.namespace).Patch(ctx, name, types.ApplyPatchType, body, metav1.PatchOptions{FieldManager: "fixture", Force: &force})
	return err
}

func fixtureStoreFactory(client func() (kubernetes.Interface, error)) func(string) (app.BootstrapIdentityStore, error) {
	return func(namespace string) (app.BootstrapIdentityStore, error) {
		return fixtureIdentityStore{client: client, namespace: namespace}, nil
	}
}
