package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/store"
)

// Both administration transports use the same catalog/cache critical section.
type lockedRepositoryRemover struct {
	database *store.Store
	cache    *gitcache.Cache
}

func (r lockedRepositoryRemover) RemoveRepository(ctx context.Context, actor, input string) (store.RemovedRepository, error) {
	var removed store.RemovedRepository
	repo, err := r.database.RepositoryByName(ctx, input)
	if err != nil {
		return removed, err
	}
	upstream, err := r.database.Upstream(ctx, repo.UpstreamID)
	if err != nil {
		return removed, err
	}
	canonical := upstream.Name + "/" + upstream.Org() + "/" + repo.Name
	err = r.cache.RemoveRepositoryWith(ctx, canonical, func() error {
		var err error
		removed, err = r.database.RemoveRepository(ctx, actor, canonical)
		return err
	})
	if err != nil {
		return removed, err
	}
	if _, err := r.database.RepositoryByName(ctx, removed.Name); errors.Is(err, store.ErrNotFound) {
		if err = r.cache.RemoveLegacyRepositoryContext(ctx, removed.Name); err != nil {
			return removed, err
		}
	}
	return removed, nil
}
func withRepositoryRemoval(server *http.Server, gate func(context.Context) error, path string, database *store.Store, cache *gitcache.Cache) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/", server.Handler)
	mux.HandleFunc("POST /v1/repository-remove", func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength != 0 || r.Header.Get("X-Oberth-Admin-Database") != path || r.Header.Get("X-Oberth-Admin-Cache-Root") != cache.CacheRoot() {
			http.Error(w, "repository removal does not match live daemon", http.StatusConflict)
			return
		}
		if gate == nil {
			http.Error(w, "audit gate unavailable", http.StatusServiceUnavailable)
			return
		}
		if err := gate(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		removed, err := (lockedRepositoryRemover{database: database, cache: cache}).RemoveRepository(r.Context(), administrativeActor, r.URL.Query().Get("repo"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(removed)
	})
	server.Handler = mux
	return server
}
func requestLiveRepositoryRemoval(ctx context.Context, path, root, input string) (store.RemovedRepository, error) {
	dialer := net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, "unix", defaultAdminSocket)
	}}
	defer transport.CloseIdleConnections()
	query := url.Values{"repo": {input}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://oberth.local/v1/repository-remove?"+query.Encode(), nil)
	if err != nil {
		return store.RemovedRepository{}, err
	}
	request.Header.Set("X-Oberth-Admin-Operation", "repository.remove")
	request.Header.Set("X-Oberth-Admin-Database", path)
	request.Header.Set("X-Oberth-Admin-Cache-Root", root)
	response, err := (&http.Client{Transport: transport, Timeout: 35 * time.Second}).Do(request)
	if err != nil {
		return store.RemovedRepository{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, maximumAdminGateError))
		return store.RemovedRepository{}, fmt.Errorf("daemon repository removal: %s: %s", response.Status, body)
	}
	var removed store.RemovedRepository
	err = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&removed)
	return removed, err
}
