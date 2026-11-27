package store

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

const receiveRefusalBurst = 8
const receiveRefusalWindow = time.Minute
const maxReceiveRefusalScopes = 1024
const maxReceiveRefusalBytes = 384 << 10
const receiveRefusalFlushTimeout = 5 * time.Second

// ReceiveRefusal contains public selectors only; no error text or workflow bytes.
type ReceiveRefusal struct {
	Code    string              `json:"code"`
	File    string              `json:"file,omitempty"`
	Step    string              `json:"step,omitempty"`
	Updates []ReceiveRefusalRef `json:"updates,omitempty"`
}

type ReceiveRefusalRef struct {
	Ref string `json:"ref"`
	Old string `json:"old_sha"`
	New string `json:"new_sha"`
}

type refusalScope struct{ actor, repository string }
type refusalWindow struct {
	start      time.Time
	recorded   int
	more       uint64
	summarized bool
}
type receiveRefusals struct {
	sync.Mutex
	windows map[refusalScope]*refusalWindow
}

// RecordReceiveRefusal allows eight detailed records and two summaries per minute
// per authenticated actor/repository. Pending counts stay in bounded memory;
// audit reads and subsequent refusals flush them without a background worker.
func (s *Store) RecordReceiveRefusal(ctx context.Context, actor, repository string, v ReceiveRefusal) error {
	raw, _ := json.Marshal(v)
	if actor == "" || repository == "" || len(raw) > maxReceiveRefusalBytes || len(v.Code) > 40 || v.Code == "" {
		return refuse(invalid, nil)
	}
	s.refusals.Lock()
	defer s.refusals.Unlock()
	key := refusalScope{actor, repository}
	w, err := s.receiveRefusalWindow(ctx, key)
	if err != nil {
		return err
	}
	if w.recorded >= receiveRefusalBurst {
		w.more++
		return nil
	}
	err = s.RecordAction(ctx, model.AuditAction{Actor: actor, Action: "receive.refused", ResourceType: "repository", ResourceID: repository, Details: string(raw)})
	if err == nil {
		w.recorded++
	}
	return err
}

func (s *Store) receiveRefusalWindow(ctx context.Context, key refusalScope) (*refusalWindow, error) {
	now := s.options.Clock()
	if s.refusals.windows == nil {
		s.refusals.windows = make(map[refusalScope]*refusalWindow)
	}
	for scope, w := range s.refusals.windows {
		if now.Sub(w.start) < receiveRefusalWindow {
			continue
		}
		if err := s.summarizeReceiveRefusals(ctx, scope, w); err != nil {
			return nil, err
		}
		delete(s.refusals.windows, scope)
	}
	if w := s.refusals.windows[key]; w != nil {
		return w, nil
	}
	if len(s.refusals.windows) >= maxReceiveRefusalScopes {
		return nil, refuse(unavailable, nil)
	}
	w := &refusalWindow{start: now}
	s.refusals.windows[key] = w
	return w, nil
}

func (s *Store) summarizeReceiveRefusals(ctx context.Context, key refusalScope, w *refusalWindow) error {
	if w.more == 0 {
		return nil
	}
	raw, _ := json.Marshal(struct {
		More uint64 `json:"more_refusals"`
	}{w.more})
	err := s.RecordAction(ctx, model.AuditAction{Actor: key.actor, Action: "receive.refused.more", ResourceType: "repository", ResourceID: key.repository, Details: string(raw)})
	if err == nil {
		w.more, w.summarized = 0, true
	}
	return err
}

func (s *Store) flushReceiveRefusals(ctx context.Context, scope []string, closing bool) error {
	s.refusals.Lock()
	defer s.refusals.Unlock()
	now := s.options.Clock()
	for key, w := range s.refusals.windows {
		if scope != nil && !slices.Contains(scope, key.repository) {
			continue
		}
		expired := now.Sub(w.start) >= receiveRefusalWindow
		if (!w.summarized || expired || closing) && w.more > 0 {
			if err := s.summarizeReceiveRefusals(ctx, key, w); err != nil {
				return err
			}
		}
		if expired {
			delete(s.refusals.windows, key)
		}
	}
	return nil
}

// FlushReceiveRefusals saves pending counts within the server's shutdown context.
func (s *Store) FlushReceiveRefusals(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, receiveRefusalFlushTimeout)
	defer cancel()
	return s.flushReceiveRefusals(ctx, nil, true)
}
