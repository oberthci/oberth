package installer

import (
	"errors"
	"fmt"
)

const maxArgoParallelism = 100000

// validateArgoLimits checks the limit flags before anything is installed.
func validateArgoLimits(cfg Config) error {
	for name, value := range map[string]*int{"--argo-parallelism": cfg.ArgoParallelism} {
		if value != nil && (*value < 0 || *value > maxArgoParallelism) {
			return fmt.Errorf("%s must be 0 (no limit) to %d", name, maxArgoParallelism)
		}
	}
	if cfg.ArgoParallelism != nil && cfg.SkipArgo {
		return errors.New("--argo-parallelism configure the managed argo-workflows release; set them on your own controller when --skip-argo-workflows is used")
	}
	return nil
}

// argoLimitsRequested reports whether this run changes any Argo limit, which
// forces a helm upgrade of an otherwise current argo-workflows release.
func argoLimitsRequested(cfg Config) bool {
	return cfg.ArgoParallelism != nil
}

// argoLimitArgs renders the requested native controller cap.
func argoLimitArgs(cfg Config) []string {
	var args []string
	if cfg.ArgoParallelism != nil {
		args = append(args, "--set", fmt.Sprintf("controller.parallelism=%d", *cfg.ArgoParallelism))
	}
	return args
}
