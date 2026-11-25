package installer

import (
	"context"
	"encoding/json"
	"io"

	"github.com/oberthci/oberth/internal/secretstore"
)

func (e *Engine) restoreStore(ctx context.Context, p *Plan, f *restoreFiles, unseal []byte) error {
	if !f.info.BundledStore {
		return nil
	}
	cfg := storeSettings(p)
	client, closeClient, err := e.storeClient(ctx, cfg)
	if err != nil {
		return problem("restore-store", nil)
	}
	defer closeClient()
	defer client.Close()
	token, err := e.initializeRestoreStore(ctx, client)
	if err != nil {
		return err
	}
	defer clear(token)
	in := f.snapshot
	if _, err = in.Seek(0, io.SeekStart); err != nil {
		return problem("restore-store", nil)
	}
	limit, _ := f.options.Limit()
	if err = client.RestoreSnapshot(ctx, token, in, limit); err != nil {
		return problem("restore-store", nil)
	}
	if err = e.unsealRestoredStore(ctx, client, unseal); err != nil {
		return err
	}
	if err = e.Credentials.save("recovery", unseal); err != nil {
		return problem("restore-store", nil)
	}
	expected, _ := json.Marshal(cfg)
	return e.Credentials.save("store-configured", []byte(publicDigest(expected)))
}

func (e *Engine) initializeRestoreStore(ctx context.Context, client *secretstore.Client) ([]byte, error) {
	status, err := readStoreStatus(ctx, client)
	if err != nil || *status.Initialized {
		return nil, problem("restore-store", nil)
	}
	var bootstrap storeBootstrap
	_, err = client.JSON(ctx, secretstore.Request{Method: "POST", Path: "/sys/init", Body: map[string]int{"secret_shares": 1, "secret_threshold": 1}}, &bootstrap)
	if err != nil || len(bootstrap.Keys) != 1 || bootstrap.Token == "" {
		return nil, problem("restore-store", nil)
	}
	share, token := []byte(bootstrap.Keys[0]), []byte(bootstrap.Token)
	defer clear(share)
	if err = e.unsealStore(ctx, client, share); err != nil {
		clear(token)
		return nil, problem("restore-store", nil)
	}
	if err = e.waitStoreActive(ctx, client); err != nil {
		clear(token)
		return nil, problem("restore-store", nil)
	}
	return token, nil
}

func (e *Engine) unsealRestoredStore(ctx context.Context, client *secretstore.Client, unseal []byte) error {
	for {
		status, err := readStoreStatus(ctx, client)
		if err == nil && *status.Sealed {
			if err = e.unsealStore(ctx, client, unseal); err != nil {
				return problem("restore-store", nil)
			}
			return e.waitStoreActive(ctx, client)
		}
		if err = e.pause(ctx); err != nil {
			return problem("restore-store", nil)
		}
	}
}
