package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/oberthci/oberth/internal/releaseverify"
	"helm.sh/helm/v3/pkg/chart/loader"
)

func checkPublishedImages(ctx context.Context, release *releaseverify.Release, dir string) error {
	body, err := release.ReadFile("oberth.tgz", releaseverify.MaxChart)
	if err != nil {
		return err
	}
	chart, err := loader.LoadArchive(bytes.NewReader(body))
	if err != nil {
		return imageRepositories
	}
	record := release.Record()
	for _, image := range []struct{ name, repository, digest string }{
		{"oberth", "imageRepository", "imageDigest"},
		{"oberth-source-plugin", "sourcePluginImageRepository", "sourcePluginImageDigest"},
	} {
		repository, _ := chart.Values[image.repository].(string)
		digest := record.Images[image.name]
		if chart.Values[image.digest] != digest {
			return binariesMismatch
		}
		if err := checkPublishedImage(ctx, repository, digest); err != nil {
			failure := unpublishedImage{error: err, image: repository + "@" + digest}
			if errors.Is(err, imageMissing) || errors.Is(err, imagePrivate) {
				failure.command = imagePushCommand(dir, image.name, repository+":"+record.Version)
			}
			return failure
		}
	}
	return nil
}

func checkPublishedImage(ctx context.Context, repository, digest string) error {
	if repository == "" {
		return imageRepositories
	}
	reference, err := name.NewDigest(repository + "@" + digest)
	if err != nil {
		return imageRepositories
	}
	descriptor, err := remote.Head(reference, remote.WithContext(ctx), remote.WithAuth(authn.Anonymous))
	if err != nil {
		var response *transport.Error
		var network net.Error
		switch {
		case errors.As(err, &response):
			if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
				return imagePrivate
			}
			if response.StatusCode == http.StatusNotFound {
				return imageMissing
			}
		case errors.As(err, &network):
			return imageLookupError{refusal: registryUnavailable, cause: err}
		}
		return imageLookupError{refusal: registryResponse, cause: err}
	}
	if descriptor.Digest.String() != digest {
		return binariesMismatch
	}
	return nil
}

type unpublishedImage struct {
	error
	image, command string
}

func (e unpublishedImage) Unwrap() error { return e.error }
func (e unpublishedImage) Remedy() string {
	if errors.Is(e.error, imageMissing) && e.command == "" {
		return "Push the image from the original build directory, then retry verification."
	}
	var message interface{ Remedy() string }
	if errors.As(e.error, &message) {
		return message.Remedy()
	}
	return registryUnavailable.Remedy()
}
func (e unpublishedImage) Detail() string {
	detail := "Image: " + e.image
	var cause interface{ Detail() string }
	if errors.As(e.error, &cause) {
		return detail + "\n" + cause.Detail()
	}
	if e.command != "" {
		return detail + "\nPush: " + e.command
	}
	return detail
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

type imageLookupError struct {
	refusal
	cause error
}

func (e imageLookupError) Detail() string { return e.cause.Error() }
func (e imageLookupError) Unwrap() error  { return e.cause }

func imagePushCommand(dir, image, target string) string {
	if dir == "" {
		return ""
	}
	layout := filepath.Join(dir, image+".oci")
	info, err := os.Stat(layout)
	if err != nil || !info.IsDir() {
		return ""
	}
	return "go tool -modfile=hack/tools/crane.mod crane push " + shellQuote(layout) + " " + shellQuote(target)
}
