package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

type MemoryScheduleState struct {
	mu    sync.Mutex
	fires map[string]map[string]time.Time
}

func NewMemoryScheduleState() *MemoryScheduleState {
	return &MemoryScheduleState{fires: map[string]map[string]time.Time{}}
}

func (state *MemoryScheduleState) ScheduleFires(_ context.Context, repo string) (map[string]time.Time, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	copied := map[string]time.Time{}
	for name, when := range state.fires[repo] {
		copied[name] = when
	}
	return copied, nil
}

func (state *MemoryScheduleState) RecordScheduleFire(_ context.Context, repo, entry string, at time.Time, outcome string) error {
	if entry == "" || (outcome != "fired" && outcome != "coalesced" && outcome != "skipped" && outcome != "failed" && outcome != "paused") {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.fires[repo] == nil {
		state.fires[repo] = map[string]time.Time{}
	}
	if outcome == "fired" || outcome == "coalesced" {
		state.fires[repo][entry] = at.UTC()
	} else if _, seen := state.fires[repo][entry]; !seen {
		state.fires[repo][entry] = time.Time{}
	}
	return nil
}

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

func isServerActor(actor string) bool { return actor == ScheduleActor }
