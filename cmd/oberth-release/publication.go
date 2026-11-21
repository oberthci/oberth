package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/releaseverify"
	"github.com/oberthci/oberth/internal/workflowpolicy"
	"golang.org/x/mod/semver"
)

type publication struct {
	origin, token, dir string
	client             *http.Client
	wait               func(context.Context, time.Duration) error
}
type latest struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}
type verification struct {
	latest
	Sums string `json:"sums"`
}

func publicationFor(o options) (publication, error) {
	token, err := os.ReadFile(o.token)
	if err != nil || len(bytes.TrimSpace(token)) == 0 {
		return publication{}, releaseToken
	}
	parsed, err := url.Parse(o.origin)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return publication{}, inputs
	}
	return publication{origin: strings.TrimRight(o.origin, "/"), token: strings.TrimSpace(string(token)), dir: o.dir, wait: waitRetry, client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (p publication) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, p.origin+"/"+path, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+p.token)
	return request, nil
}
func (p publication) createFiles(ctx context.Context, version string) error {
	entries, err := os.ReadDir(p.dir)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".oci") && entry.IsDir() {
			continue
		}
		if !entry.Type().IsRegular() {
			return inputs
		}
		names = append(names, entry.Name())
	}
	slices.SortFunc(names, func(a, b string) int { return publicationRank(a) - publicationRank(b) })
	for _, name := range names {
		if err := p.createFile(ctx, version, name); err != nil {
			return err
		}
	}

	return nil
}
func (p publication) createFile(ctx context.Context, version, name string) error {
	file, err := os.Open(filepath.Join(p.dir, name))
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	request, err := p.request(ctx, http.MethodPut, version+"/"+name, file)
	if err != nil {
		return err
	}
	request.ContentLength = info.Size()
	request.Header.Set("If-None-Match", "*")
	response, err := p.client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusPreconditionFailed {
		return p.equalFile(ctx, version+"/"+name, name, info.Size())
	}
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		return storeFailure
	}
	return nil
}
func (p publication) equalFile(ctx context.Context, path, name string, size int64) error {
	request, err := p.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	response, err := p.client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return storeFailure
	}
	sum := sha256.New()
	n, err := io.Copy(sum, io.LimitReader(response.Body, size+1))
	if err != nil {
		return err
	}
	want, err := hashFile(ctx, filepath.Join(p.dir, name))
	if err != nil {
		return err
	}
	if n != size || fmt.Sprintf("%x", sum.Sum(nil)) != want {
		return immutable
	}
	return nil
}
func (p publication) current(ctx context.Context) (latest, string, error) {
	var value latest
	request, err := p.request(ctx, http.MethodGet, "latest", nil)
	if err != nil {
		return value, "", err
	}
	response, err := p.client.Do(request)
	if err != nil {
		return value, "", err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusForbidden {
		return value, "", nil
	}
	if response.StatusCode != http.StatusOK {
		return value, "", storeFailure
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, releaseverify.MaxMetadata+1))
	if err != nil {
		return value, "", err
	}
	if len(body) > releaseverify.MaxMetadata || json.Unmarshal(body, &value) != nil || !workflowpolicy.IsReleaseVersion(value.Version) || response.Header.Get("ETag") == "" {
		return value, "", latestInvalid
	}
	return value, response.Header.Get("ETag"), nil
}
func (p publication) advance(ctx context.Context, want latest) (latest, error) {
	if !workflowpolicy.IsReleaseVersion(want.Version) {
		return latest{}, inputs
	}
	body, err := json.Marshal(want)
	if err != nil {
		return latest{}, err
	}
	for attempt := 0; attempt < 5; attempt++ {
		current, etag, err := p.current(ctx)
		if err != nil {
			return latest{}, err
		}
		switch semver.Compare(current.Version, want.Version) {
		case 1:
			return current, nil
		case 0:
			if current.Commit != want.Commit {
				return latest{}, immutable
			}
			return current, nil
		}
		retry, err := p.compareAndSwap(ctx, body, etag)
		if err != nil {
			return latest{}, err
		}
		if !retry {
			return want, nil
		}
		if err := p.backoff(ctx, attempt); err != nil {
			return latest{}, err
		}
	}
	return latest{}, latestBusy
}
func (p publication) compareAndSwap(ctx context.Context, body []byte, etag string) (bool, error) {
	request, err := p.request(ctx, http.MethodPut, "latest", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	if etag == "" {
		request.Header.Set("If-None-Match", "*")
	} else {
		request.Header.Set("If-Match", etag)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return false, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusPreconditionFailed {
		return true, nil
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return false, storeFailure
	}
	return false, nil
}
func writeVerification(ctx context.Context, o options, record releaseverify.Record) error {
	sums, err := hashFile(ctx, filepath.Join(o.dir, "SHA256SUMS"))
	if err != nil {
		return err
	}
	body, err := json.Marshal(verification{latest: latest{Version: record.Version, Commit: record.Commit}, Sums: sums})
	if err != nil {
		return err
	}
	return os.WriteFile(o.receipt, body, 0600)
}
func verified(ctx context.Context, o options, record releaseverify.Record) error {
	body, err := readSmallFile(o.receipt)
	if err != nil {
		return notVerified
	}
	var proof verification
	sums, err := hashFile(ctx, filepath.Join(o.dir, "SHA256SUMS"))
	if err != nil || json.Unmarshal(body, &proof) != nil || proof.Version != record.Version || proof.Commit != record.Commit || proof.Sums != sums {
		return notVerified
	}
	return nil
}
func publish(ctx context.Context, o options) error {
	p, err := publicationFor(o)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(o.dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	release, err := releaseverify.Verify(ctx, root.FS(), o.version)
	if err != nil {
		return err
	}
	record := release.Record()
	if err := checkPublishedImages(ctx, release, o.dir); err != nil {
		return err
	}
	staged, err := os.MkdirTemp("", "publication-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staged) }()
	if err := stagePublication(release, staged); err != nil {
		return err
	}
	if !workflowpolicy.IsReleaseVersion(record.Version) {
		return inputs
	}
	p.dir = staged
	if err := p.publishFiles(ctx, record.Version); err != nil {
		return err
	}
	_, err = fmt.Printf("Published %s: %d files at %s/%s.\n", record.Version, len(publishedNames())+2, p.origin, record.Version)
	return err
}
func finalize(ctx context.Context, o options) error {
	p, err := publicationFor(o)
	if err != nil {
		return err
	}
	record, err := loadVerified(ctx, o)
	if err != nil {
		return err
	}
	if err := verified(ctx, o, record); err != nil {
		return err
	}
	return p.finish(ctx, latest{Version: record.Version, Commit: record.Commit}, os.Stdout)
}

func (p publication) finish(ctx context.Context, want latest, output io.Writer) error {
	current, err := p.advance(ctx, want)
	if err != nil {
		return err
	}
	if current.Version != want.Version {
		_, err = fmt.Fprintf(output, "Latest at %s/latest stays %s; %s was not made latest.\n", p.origin, current.Version, want.Version)
	} else {
		_, err = fmt.Fprintf(output, "Latest at %s/latest is %s.\n", p.origin, current.Version)
	}
	return err
}

func readSmallFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, releaseverify.MaxMetadata+1))
	if err != nil {
		return nil, err
	}
	if len(body) > releaseverify.MaxMetadata {
		return nil, releaseverify.Limit
	}
	return body, nil
}

func publicationRank(name string) int {
	switch name {
	case "release.json":
		return 1
	case "SHA256SUMS":
		return 2
	case "SHA256SUMS.sigstore.json":
		return 3
	}
	return 0
}
func waitRetry(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (p publication) backoff(ctx context.Context, attempt int) error {
	if attempt == 4 {
		return nil
	}
	return p.wait(ctx, 100*time.Millisecond<<attempt)
}
