package storetest

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func fixtureDB(database *store.Store) (*sql.DB, fixtureInfo, error) {
	value, ok := fixtures.Load(database)
	if !ok {
		return nil, fixtureInfo{}, fmt.Errorf("unregistered store fixture")
	}
	fixture := value.(fixtureInfo)
	uri := url.URL{Scheme: "file", Path: fixture.path}
	db, err := sql.Open("sqlite", uri.String()+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err == nil {
		db.SetMaxOpenConns(1)
	}
	return db, fixture, err
}
func fixtureNow(fixture fixtureInfo) time.Time {
	if fixture.now != nil {
		return fixture.now()
	}
	return time.Now()
}
func fixtureNotFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	return err
}

// CreateUpstream is explicit seed data, without claiming to test registration.
func CreateUpstream(database *store.Store, ctx context.Context, spec model.UpstreamSpec) (model.Upstream, error) {
	if strings.TrimSpace(spec.Name) == "" || strings.TrimSpace(spec.Kind) == "" || strings.TrimSpace(spec.BaseURL) == "" {
		return model.Upstream{}, store.ErrInvalid
	}
	db, fixture, err := fixtureDB(database)
	if err != nil {
		return model.Upstream{}, err
	}
	defer func() { _ = db.Close() }()
	stamp := fixtureNow(fixture).UnixNano()
	result, err := db.ExecContext(ctx, `INSERT INTO upstreams(name,kind,base_url,key_name,created_at,updated_at) VALUES(?,?,?,?,?,?)`, spec.Name, spec.Kind, spec.BaseURL, spec.KeyName, stamp, stamp)
	if err != nil {
		return model.Upstream{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return model.Upstream{}, err
	}
	return database.Upstream(ctx, id)
}
func UpstreamByName(database *store.Store, ctx context.Context, name string) (model.Upstream, error) {
	values, err := database.ListUpstreams(ctx)
	if err != nil {
		return model.Upstream{}, err
	}
	for _, value := range values {
		if value.Name == name {
			return value, nil
		}
	}
	return model.Upstream{}, store.ErrNotFound
}
func UpsertUplink(database *store.Store, ctx context.Context, spec model.UplinkSpec) (model.Uplink, error) {
	return database.RegisterUplink(ctx, spec.AuthActor, spec)
}

// EnqueueRun seeds historical runs without inventing a receive event. Live
// receive semantics are exercised separately through EnqueueReceiveEvent.
func EnqueueRun(database interface {
	EnqueueRun(context.Context, model.RunSpec) (model.EnqueueRunResult, error)
}, ctx context.Context, spec model.RunSpec) (model.EnqueueRunResult, error) {
	return database.EnqueueRun(ctx, spec)
}
func EnqueuePromotionRun(database *store.Store, ctx context.Context, run model.RunSpec, spec model.PromotionSpec) (model.EnqueueRunResult, model.Promotion, error) {
	promotion, err := database.AppendPromotion(ctx, spec)
	if err != nil {
		return model.EnqueueRunResult{}, model.Promotion{}, err
	}
	return database.EnqueueAdmittedPromotionRun(ctx, run, promotion.ID)
}
func LatestRunForRef(database *store.Store, ctx context.Context, repoID int64, ref string) (model.Run, error) {
	db, _, err := fixtureDB(database)
	if err != nil {
		return model.Run{}, err
	}
	defer func() { _ = db.Close() }()
	var id string
	if err = db.QueryRowContext(ctx, `SELECT id FROM runs WHERE repo_id=? AND ref=? ORDER BY queue_sequence DESC LIMIT 1`, repoID, ref).Scan(&id); err != nil {
		return model.Run{}, fixtureNotFound(err)
	}
	return database.Run(ctx, id)
}
func TokenCredentialByDigest(database *store.Store, ctx context.Context, digest []byte) (model.TokenCredential, error) {
	if len(digest) != 32 {
		return model.TokenCredential{}, store.ErrNotFound
	}
	db, _, err := fixtureDB(database)
	if err != nil {
		return model.TokenCredential{}, err
	}
	defer func() { _ = db.Close() }()
	var value model.TokenCredential
	var created int64
	var activated, used, revoked, expiry sql.NullInt64
	if err = db.QueryRowContext(ctx, `SELECT id,name,digest,created_at,activated_at,last_used_at,revoked_at,expires_at FROM token_credentials WHERE digest=?`, digest).Scan(&value.ID, &value.Name, &value.Digest, &created, &activated, &used, &revoked, &expiry); err != nil {
		return model.TokenCredential{}, fixtureNotFound(err)
	}
	value.CreatedAt = time.Unix(0, created).UTC()
	value.ActivatedAt = fixtureTime(activated)
	value.LastUsedAt = fixtureTime(used)
	value.RevokedAt = fixtureTime(revoked)
	value.ExpiresAt = fixtureTime(expiry)
	return value, nil
}
func fixtureTime(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	at := time.Unix(0, value.Int64).UTC()
	return &at
}
func RevokeTokenCredential(database *store.Store, ctx context.Context, id string) error {
	db, _, err := fixtureDB(database)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	var identity, fingerprint string
	if err = db.QueryRowContext(ctx, `SELECT identity,fingerprint FROM uplinks WHERE token_credential_id=?`, id).Scan(&identity, &fingerprint); err != nil {
		return fixtureNotFound(err)
	}
	_, err = database.RevokeUplink(ctx, "fixture", identity, fingerprint, "")
	return err
}

// ReleaseIssueLock explicitly advances a fixture past an investigation lock;
// production intentionally relies on expiry and exposes no release capability.
func ReleaseIssueLock(database *store.Store, ctx context.Context, id int64, owner string) error {
	db, _, err := fixtureDB(database)
	if err != nil {
		return err
	}
	defer db.Close()
	var acquired int64
	if err = db.QueryRowContext(ctx, `DELETE FROM issue_locks WHERE issue_id=? AND owner=? RETURNING acquired_at`, id, owner).Scan(&acquired); errors.Is(err, sql.ErrNoRows) {
		return store.ErrLockNotOwned
	} else if err != nil {
		return err
	}
	details, _ := json.Marshal(map[string]any{"acquired_at": time.Unix(0, acquired).UTC().Format(time.RFC3339Nano)})
	_, err = database.AppendAuditAction(ctx, model.AuditActionSpec{Actor: owner, Action: "issue.lock.release", ResourceType: "issue", ResourceID: fmt.Sprint(id), Details: string(details)})
	return err
}

// ConsumePromotionGrant seeds historical consumed status for list fixtures.
func ConsumePromotionGrant(database *store.Store, ctx context.Context, id, promotionID, actor string) error {
	db, fixture, err := fixtureDB(database)
	if err != nil {
		return err
	}
	defer db.Close()
	now := fixtureNow(fixture).UnixNano()
	result, err := db.ExecContext(ctx, `UPDATE promotion_grants SET consumed_by=?,consumed_at=? WHERE id=? AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at>?`, promotionID, now, id, now)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return store.ErrInvalidState
	}
	details, _ := json.Marshal(map[string]string{"promotion_id": promotionID})
	_, err = database.AppendAuditAction(ctx, model.AuditActionSpec{Actor: actor, Action: "promotion_grant.consume", ResourceType: "promotion_grant", ResourceID: id, Details: string(details)})
	return err
}
