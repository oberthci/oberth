package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

func checkUpgradeLegacyRepositories(ctx context.Context, cfg UpgradeConfig, deps Deps) error {
	if cfg.AllowRetiredExecution {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	run := deps.RunCommand
	if run == nil {
		run = DefaultRunCommand
	}
	args := []string{"exec", "-c", "oberth", "-n", cfg.Namespace, "deploy/oberth", "--", "/usr/local/bin/oberth", "admin", "retirement-check"}
	if deps.ContextName != "" {
		args = append(args[:1], append([]string{"--context", deps.ContextName}, args[1:]...)...)
	}
	body, err := run(ctx, nil, "kubectl", args...)
	if len(body) > 4096 {
		return errors.New("upgrade refused: retirement proof response exceeded 4096 bytes")
	}
	if err != nil {
		return fmt.Errorf("upgrade refused: live repository retirement check failed; retain or install the prerequisite check release with the legacy recovery engine, migrate named repositories and drain before retrying (explicit override: --allow-retired-execution): %w: %s", err, strings.TrimSpace(string(body)))
	}
	var proof struct {
		Format string `json:"format"`
		Ready  bool   `json:"ready"`
	}
	if json.Unmarshal(body, &proof) != nil || proof.Format != "oberth-retirement-check-v1" || !proof.Ready {
		return errors.New("upgrade refused: live daemon did not provide a complete repository retirement proof; install the prerequisite check release first (explicit override: --allow-retired-execution)")
	}
	return nil
}
