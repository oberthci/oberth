package main

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/releaseverify"
)

func installationRelease(ctx context.Context, request installationRequest) (installer.Release, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if chart := request.text("chart"); chart != "" {
		return installer.LocalRelease(chart)
	}
	version := request.text("version")
	if version == "" && binaryVersion() != "(devel)" {
		version = binaryVersion()
	}
	files, err := installationReleaseFiles(ctx, request, version)
	if err != nil {
		return installer.Release{}, err
	}
	return installer.LoadRelease(ctx, files, version)
}

func installationReleaseFiles(ctx context.Context, request installationRequest, version string) (fs.FS, error) {
	if directory := request.text("release-dir"); directory != "" {
		return localInstallationReleaseFiles(directory, version)
	}
	return releaseHTTPFiles{ctx: ctx, version: version}, nil
}

func localInstallationReleaseFiles(directory, version string) (fs.FS, error) {
	files := os.DirFS(directory)
	if version != "" {
		if _, err := fs.Stat(files, version); err == nil {
			files, err = fs.Sub(files, version)
			if err != nil {
				return nil, localReleaseFilesProblem(directory, version, err)
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, localReleaseFilesProblem(directory, version, err)
		}
	}
	if _, err := fs.Stat(files, "release.json"); err != nil {
		return nil, localReleaseFilesProblem(directory, version, err)
	}
	return localReleaseFiles{FS: files, directory: directory, version: version}, nil
}

const localReleaseFilesID = "local-release-files"

type localReleaseFiles struct {
	fs.FS
	directory, version string
}

// ReleaseProblem preserves lookup guidance after canonical version verification.
func (f localReleaseFiles) ReleaseProblem(problem *installer.Problem) {
	if errors.Is(problem, releaseverify.Version) {
		lookup := localReleaseFilesProblem(f.directory, f.version, problem)
		problem.Remedy = lookup.Remedy()
		problem.Detail = strings.Join(lookup.Context, "\n")
	}
}

func localReleaseFilesProblem(directory, version string, cause error) *publicerr.Error {
	reason, remedy := "Release directory cannot be read.", "Check the searched directories and their read permissions."
	if errors.Is(cause, fs.ErrNotExist) || errors.Is(cause, releaseverify.Version) {
		reason, remedy = "Required release files were not found.", "Place the signed release in a searched directory, then retry."
	}
	problem := publicerr.New(publicerr.Unavailable, reason, remedy, cause)
	problem.ID = localReleaseFilesID
	problem.Context = []string{"Required release version: " + version,
		"Searched version directory: " + filepath.Join(directory, version), "Searched flat directory: " + directory}
	return problem
}

// Only the owner verifier decides trust and bounds; this adapter retrieves bytes.
type releaseHTTPFiles struct {
	ctx       context.Context
	version   string
	transport http.RoundTripper
}

func (f releaseHTTPFiles) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) || strings.Contains(name, "/") || !fs.ValidPath(f.version) || strings.Contains(f.version, "/") {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	address := releaseverify.Origin + "/" + url.PathEscape(f.version) + "/" + url.PathEscape(name)
	request, err := http.NewRequestWithContext(f.ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	client := http.Client{Transport: f.transport, Timeout: 60 * time.Second, CheckRedirect: releaseRedirect}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		return nil, &fs.PathError{Op: "read", Path: name, Err: fs.ErrNotExist}
	}
	return releaseHTTPFile{ReadCloser: response.Body, name: name}, nil
}

const releaseRedirectID = "release-redirect"

func releaseRedirect(request *http.Request, _ []*http.Request) error {
	problem := publicerr.New(publicerr.Unavailable, "The release address redirected.", "Use the direct public release address and retry.", nil)
	problem.ID = releaseRedirectID
	problem.Context = []string{"Redirect target: " + request.URL.Redacted()}
	return problem
}

type releaseHTTPFile struct {
	io.ReadCloser
	name string
}

func (f releaseHTTPFile) Stat() (fs.FileInfo, error) { return releaseFileInfo{f.name}, nil }

type releaseFileInfo struct {
	name string
}

func (f releaseFileInfo) Name() string       { return f.name }
func (f releaseFileInfo) Size() int64        { return -1 }
func (f releaseFileInfo) Mode() fs.FileMode  { return 0444 }
func (f releaseFileInfo) ModTime() time.Time { return time.Time{} }
func (f releaseFileInfo) IsDir() bool        { return false }
func (f releaseFileInfo) Sys() any           { return nil }
