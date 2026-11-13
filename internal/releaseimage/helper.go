package releaseimage

import (
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

func helperContract() imageContract {
	return imageContract{
		kind: Helper, repository: HelperRepository, substrate: ServerSubstrate,
		binaryName: "oberth", imagePath: "usr/local/bin/oberth",
		platforms: []string{"amd64", "arm64"},
		required:  []string{"etc/ssl/certs/ca-certificates.crt"},
		config: v1.Config{
			User: "65534:65534", Env: []string{"PATH=/usr/local/bin"},
			Entrypoint: []string{"/usr/local/bin/oberth"}, WorkingDir: "/",
		},
	}
}

// The helper carries the release executable and public trust roots only.
func helperRuntimePath(name string) bool {
	for _, file := range []string{"usr/local/bin/oberth", "etc/ssl/certs/ca-certificates.crt"} {
		if name == file || strings.HasPrefix(file, name+"/") {
			return true
		}
	}
	return false
}
