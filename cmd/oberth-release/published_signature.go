package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"path/filepath"

	"github.com/oberthci/oberth/internal/releaseverify"
)

func (p publication) publishFiles(ctx context.Context, version string) error {
	if err := p.checkSignature(ctx, version); err != nil {
		return err
	}
	return p.createFiles(ctx, version)
}

func (p publication) checkSignature(ctx context.Context, version string) error {
	const name = "SHA256SUMS.sigstore.json"
	request, err := p.request(ctx, http.MethodGet, version+"/"+name, nil)
	if err != nil {
		return err
	}
	response, err := p.client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	switch response.StatusCode {
	case http.StatusForbidden, http.StatusNotFound:
		return nil
	case http.StatusOK:
	default:
		return storeFailure
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, releaseverify.MaxMetadata+1))
	if err != nil {
		return err
	}
	local, err := readSmallFile(filepath.Join(p.dir, name))
	if err != nil {
		return err
	}
	if !bytes.Equal(body, local) {
		return immutable
	}
	return nil
}
