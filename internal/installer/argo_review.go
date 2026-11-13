package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

const argoValuesRemedy = "Read the Argo release values before reviewing controller.parallelism and controller.namespaceParallelism."

// These values are for review only. Reading an operator's limit must never
// turn it into an explicit override during a later Helm reconciliation.
func readArgoReviewValues(ctx context.Context, cfg *Config, deps Deps) error {
	if cfg.SkipArgo || cfg.argoValuesRead {
		return nil
	}
	ns := cfg.ArgoNamespace
	if ns == "" {
		ns = DefaultArgoNamespace
	}
	exists := cfg.existingArgoRelease
	if !cfg.argoReleaseRead {
		_, found, err := findHelmRelease(ctx, deps, "argo-workflows", ns)
		if err != nil {
			return err
		}
		exists = found
		cfg.existingArgoRelease, cfg.argoReleaseRead = exists, true
	}
	if exists {
		raw, err := deps.RunHelm(ctx, []string{"get", "values", "argo-workflows", "-n", ns, "--all", "-o", "json"})
		if err != nil {
			return fmt.Errorf("read Argo controller values: %w", err)
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if !json.Valid(raw) || decoder.Decode(&cfg.liveArgoValues) != nil {
			return errors.New("cannot decode Argo controller values")
		}
	}
	cfg.existingArgoRelease, cfg.argoValuesRead = exists, true
	return nil
}

func argoReviewLimit(cfg Config, requested *int, key string, fresh int) string {
	if cfg.SkipArgo {
		return "external controller"
	}
	if requested != nil {
		return strconv.Itoa(*requested) + " (requested)"
	}
	if cfg.existingArgoRelease {
		value := valueAt(cfg.liveArgoValues, "controller", key)
		if value == nil {
			return "0 (preserved)"
		}
		if number, ok := value.(json.Number); ok {
			if n, err := number.Int64(); err == nil && n >= 0 {
				return strconv.FormatInt(n, 10) + " (preserved)"
			}
		}
		return "stored value (preserved)"
	}
	if cfg.argoValuesRead {
		return strconv.Itoa(fresh) + " (fresh default)"
	}
	return strconv.Itoa(fresh) + " for a new release; preserve existing value"
}
