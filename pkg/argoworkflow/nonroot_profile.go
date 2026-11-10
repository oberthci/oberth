package argoworkflow

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// NonrootStaticProfile is the single built-in controller configuration whose
// container/script generation is covered by the pinned Argo 4.0.8 fixtures.
// Selecting its name is not execution proof: the server verifies live identity,
// immutable configuration and freshness before each selected submission.
const NonrootStaticProfile = "nonroot-static-v1"

// NonrootProfileConfig is the canonical safe config rendered by the official
// argo-workflows 1.0.24 chart defaults with the server disabled. No archive,
// default-template, plugin, main/executor or credential configuration is allowed.
const NonrootProfileConfig = `{"failedPodRestart":{"enabled":false,"maxRestarts":3},"nodeEvents":{"enabled":true},"workflowEvents":{"enabled":true}}`

// NonrootProfileConfigSHA256 pins the complete canonical configuration.
const NonrootProfileConfigSHA256 = "5f121f7f1f5ef468eb2208b1cf7e911746c8cb6173c1d24e7374ad951af92d59"

// NonrootProfileConfigMap uses 192 bits of the content digest within a DNS label;
// verification additionally compares the complete digest and exact data bytes.
const NonrootProfileConfigMap = "oberth-argo-nr-5f121f7f1f5ef468eb2208b1cf7e911746c8cb6173c1d24e"

// NonrootProfileVolume and NonrootProfileMount belong only to the trusted
// controller. The required public config key gates startup before Argo's
// synchronous API config load; neither is mounted in a workflow Pod.
const NonrootProfileVolume = "oberth-controller-profile"
const NonrootProfileMount = "/var/run/oberth-controller-profile"

// NonrootControllerImage pins the official multi-platform controller index.
const NonrootControllerImage = "quay.io/argoproj/workflow-controller:v4.0.8@sha256:7a156419f80285859fc8f859927b6cc249f0d128e161e080dc17fca8fcbbceb6"

// NonrootExecutorImage pins the official multi-platform executor index.
const NonrootExecutorImage = "quay.io/argoproj/argoexec:v4.0.8@sha256:86a965d7eea176959351156e24f3a2fa8d8e342e477ef425009af95a12e3fe87"

// NonrootControllerAMD64 is the authenticated amd64 member of the pinned index.
// Initial runtime verification accepts that member or the exact index imageID;
// no other native-architecture execution is claimed by the current proof.
const NonrootControllerAMD64 = "quay.io/argoproj/workflow-controller@sha256:bba570982ca563738f327685562a04cdce771f18993f37e17b751a520d50c20e"

// MaxNonrootParallelism bounds the controller parallelism a profile carries.
const MaxNonrootParallelism = 100000

// nonrootProfilePrefix starts every profile ConfigMap name; 48 hexadecimal
// digest characters follow it.
const nonrootProfilePrefix = "oberth-argo-nr-"

// NonrootProfile is one canonical controller configuration: the built-in
// config plus, when positive, the controller's own run limits
// (parallelism, namespaceParallelism; #1517). Every value derives from the
// two integers, so the content digest, the immutable ConfigMap name and the
// exact data bytes are one deterministic function of them. Zero limits give
// exactly NonrootProfileConfig, NonrootProfileConfigSHA256 and
// NonrootProfileConfigMap.
type NonrootProfile struct {
	Parallelism          int
	NamespaceParallelism int
	Config               string
	SHA256               string
	ConfigMap            string
}

// NewNonrootProfile returns the canonical profile for the given limits; 0 is
// no limit.
func NewNonrootProfile(parallelism, namespaceParallelism int) (NonrootProfile, error) {
	if parallelism < 0 || parallelism > MaxNonrootParallelism || namespaceParallelism < 0 || namespaceParallelism > MaxNonrootParallelism {
		return NonrootProfile{}, fmt.Errorf("argoworkflow: controller parallelism must be 0..%d", MaxNonrootParallelism)
	}
	config := map[string]any{
		"failedPodRestart": map[string]any{"enabled": false, "maxRestarts": 3},
		"nodeEvents":       map[string]any{"enabled": true},
		"workflowEvents":   map[string]any{"enabled": true},
	}
	if parallelism > 0 {
		config["parallelism"] = parallelism
	}
	if namespaceParallelism > 0 {
		config["namespaceParallelism"] = namespaceParallelism
	}
	// Maps marshal with sorted keys, so the bytes are canonical.
	encoded, err := json.Marshal(config)
	if err != nil {
		return NonrootProfile{}, err
	}
	digest := sha256.Sum256(encoded)
	sum := hex.EncodeToString(digest[:])
	return NonrootProfile{Parallelism: parallelism, NamespaceParallelism: namespaceParallelism,
		Config: string(encoded), SHA256: sum, ConfigMap: nonrootProfilePrefix + sum[:48]}, nil
}

// ValidNonrootProfileConfigMap reports whether name has the profile name form.
func ValidNonrootProfileConfigMap(name string) bool {
	digest, ok := strings.CutPrefix(name, nonrootProfilePrefix)
	if !ok || len(digest) != 48 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil && strings.ToLower(digest) == digest
}

// ParseNonrootProfile accepts exactly a canonical profile: config must be the
// built-in configuration plus at most positive parallelism and
// namespaceParallelism, byte for byte as NewNonrootProfile renders it, and
// name its digest-derived ConfigMap name. Anything else is refused.
func ParseNonrootProfile(name, config string) (NonrootProfile, error) {
	var limits struct {
		FailedPodRestart     json.RawMessage `json:"failedPodRestart"`
		NodeEvents           json.RawMessage `json:"nodeEvents"`
		WorkflowEvents       json.RawMessage `json:"workflowEvents"`
		Parallelism          int             `json:"parallelism"`
		NamespaceParallelism int             `json:"namespaceParallelism"`
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(config)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&limits); err != nil {
		return NonrootProfile{}, errors.New("argoworkflow: controller profile config is not the built-in configuration")
	}
	profile, err := NewNonrootProfile(limits.Parallelism, limits.NamespaceParallelism)
	if err != nil {
		return NonrootProfile{}, err
	}
	if profile.Config != config || profile.ConfigMap != name {
		return NonrootProfile{}, errors.New("argoworkflow: controller profile is not a canonical built-in profile")
	}
	return profile, nil
}
