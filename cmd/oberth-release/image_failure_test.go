package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUncreatedOrPrivateRepositoryExplainsBothRemedies(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v2/" {
					w.WriteHeader(http.StatusOK)
					return
				}
				w.WriteHeader(code)
			}))
			defer server.Close()
			err := checkPublishedImage(t.Context(), strings.TrimPrefix(server.URL, "http://")+"/uncreated", "sha256:"+strings.Repeat("a", 64))
			if !errors.Is(err, imagePrivate) || imagePrivate.Error() != "Release image is absent or private." || imagePrivate.Remedy() != "Push the image if it is absent, then allow public reads; see Detail." {
				t.Fatal("unauthorized repository did not explain absence and private reads", err)
			}
		})
	}
}

func TestRegistryResponseRetainsCauseWithoutInvitingAnotherPush(t *testing.T) {
	for _, code := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusOK} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v2/" {
					w.WriteHeader(http.StatusOK)
					return
				}
				w.WriteHeader(code)
			}))
			defer server.Close()
			reference := strings.TrimPrefix(server.URL, "http://") + "/oberth"
			err := checkPublishedImage(t.Context(), reference, "sha256:"+strings.Repeat("a", 64))
			var lookup imageLookupError
			if !errors.As(err, &lookup) || lookup.Error() != registryResponse.Error() || lookup.Detail() == "" {
				t.Fatal("registry response became a network fault", err)
			}
			refusal := unpublishedImage{error: err, image: reference, command: "must not appear"}
			if !strings.Contains(refusal.Detail(), lookup.Detail()) || strings.Contains(refusal.Detail(), "Push:") || !strings.Contains(refusal.Detail(), reference) {
				t.Fatal("wrapper hid the registry response or suggested a push", refusal.Detail())
			}
		})
	}
}

func TestImagePushRemedyRequiresAnExistingLayout(t *testing.T) {
	dir := t.TempDir()
	for _, kind := range []string{"no build directory", "missing", "file", "directory"} {
		t.Run(kind, func(t *testing.T) { checkLayoutRemedy(t, dir, kind) })
	}
}
func checkLayoutRemedy(t *testing.T, dir, kind string) {
	t.Helper()

	root := dir
	if kind == "no build directory" {
		root = ""
	}
	path := filepath.Join(dir, kind+".oci")
	if kind == "file" {
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if kind == "directory" {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	command := imagePushCommand(root, kind, "repo:v1.2.3")
	failure := unpublishedImage{error: imageMissing, image: "repo@digest", command: command}
	if (command != "") != (kind == "directory") {
		t.Fatal("push command availability differs", command)
	}
	if command == "" && (failure.Detail() != "Image: repo@digest" || !strings.Contains(failure.Remedy(), "original build directory")) {
		t.Fatal("missing layout guidance differs", failure.Detail(), failure.Remedy())
	}
	if len(failure.Remedy()) > 120 {
		t.Fatal("remedy exceeds its budget")
	}
}
