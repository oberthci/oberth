package api

import (
	"context"
	"mime"
	"net/http"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
)

func (s *Server) accountBackup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.draining.Load() {
		s.writeError(w, r, refusal("restarting"), 503)
		return
	}
	if s.config.AccountBackup == nil {
		s.writeError(w, r, refusal("forbidden"), 403)
		return
	}
	token, err := bearerToken(r, false)
	if err != nil || (r.TLS != nil && len(r.TLS.PeerCertificates) != 0) {
		s.writeError(w, r, refusal("auth"), 401)
		return
	}
	media, _, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaErr != nil || media != "application/json" || r.URL.RawQuery != "" {
		s.writeError(w, r, refusal("media"), 400)
		return
	}
	raw, err := readBody(w, r, apicontract.AdminBytes)
	var empty apicontract.Empty
	if err != nil || decode(raw, &empty, apicontract.AdminBytes) != nil {
		s.writeError(w, r, refusal("input"), 400)
		return
	}
	observing, stop := s.observationContext(r.Context())
	defer stop()
	ctx, cancel := context.WithTimeout(observing, 10*time.Minute)
	defer cancel()
	source, err := s.config.AccountBackup.Snapshot(ctx, token)
	if err != nil {
		s.writeError(w, r, err, 0)
		return
	}
	joined := make(chan struct{})
	go func() { defer close(joined); <-ctx.Done(); _ = source.Close() }()
	defer func() { cancel(); <-joined }()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="oberth-database-backup.tar"`)
	_ = s.copyStream(w, source)
}
