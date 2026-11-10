package releaseverify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Default publication origins, the ones release.sh publishes to.
const (
	DefaultReleaseOrigin   = "https://releases.oberth.ci"
	DefaultChartRepository = "https://charts.oberth.ci"
)

// Getter fetches one URL with a byte limit.
type Getter func(ctx context.Context, rawURL string, limit int64) ([]byte, error)

// HTTPSGetter is the default Getter: HTTPS only, no redirects to another
// scheme, bounded body and time.
func HTTPSGetter(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return nil, fmt.Errorf("refusing non-HTTPS release URL %q", rawURL)
	}
	client := &http.Client{
		Timeout: 2 * time.Minute,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if request.URL.Scheme != "https" || len(via) > 5 {
				return errors.New("refusing a release redirect off HTTPS")
			}
			return nil
		},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", rawURL, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d", rawURL, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", rawURL, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("fetch %s: larger than %d bytes", rawURL, limit)
	}
	return body, nil
}

// Fetch downloads the four published objects of tag.
func Fetch(ctx context.Context, get Getter, releaseOrigin, chartRepository, tag string) (Files, error) {
	if get == nil {
		get = HTTPSGetter
	}
	if releaseOrigin == "" {
		releaseOrigin = DefaultReleaseOrigin
	}
	if chartRepository == "" {
		chartRepository = DefaultChartRepository
	}
	record, recordBundle, chart, chartBundle := Names(tag)
	releaseBase := strings.TrimRight(releaseOrigin, "/") + "/oberth/" + tag + "/"
	chartBase := strings.TrimRight(chartRepository, "/") + "/"
	var files Files
	for _, item := range []struct {
		url   string
		limit int64
		into  *[]byte
	}{
		{releaseBase + record, MaxRecordBytes, &files.Record},
		{releaseBase + recordBundle, MaxBundleBytes, &files.RecordBundle},
		{chartBase + chart, MaxChartBytes, &files.Chart},
		{chartBase + chartBundle, MaxBundleBytes, &files.ChartBundle},
	} {
		body, err := get(ctx, item.url, item.limit)
		if err != nil {
			return Files{}, err
		}
		*item.into = body
	}
	return files, nil
}

// LoadDir reads the four objects of tag from a directory an operator
// downloaded them to (air-gapped hosts): release.json,
// release.json.sigstore.json, oberth-<version>.tgz and its .sigstore.json.
func LoadDir(dir, tag string) (Files, error) {
	record, recordBundle, chart, chartBundle := Names(tag)
	var files Files
	for _, item := range []struct {
		name  string
		limit int64
		into  *[]byte
	}{
		{record, MaxRecordBytes, &files.Record},
		{recordBundle, MaxBundleBytes, &files.RecordBundle},
		{chart, MaxChartBytes, &files.Chart},
		{chartBundle, MaxBundleBytes, &files.ChartBundle},
	} {
		path := filepath.Join(dir, item.name)
		info, err := os.Lstat(path)
		if err != nil {
			return Files{}, fmt.Errorf("release file %s: %w", path, err)
		}
		if !info.Mode().IsRegular() || info.Size() > item.limit {
			return Files{}, fmt.Errorf("release file %s is not a regular file within %d bytes", path, item.limit)
		}
		body, err := os.ReadFile(path) // #nosec G304 -- operator-chosen release directory, fixed member names.
		if err != nil {
			return Files{}, err
		}
		*item.into = body
	}
	return files, nil
}
