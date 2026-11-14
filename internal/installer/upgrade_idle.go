package installer

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"golang.org/x/term"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

func waitBeforeUpgrade(ctx context.Context, cfg UpgradeConfig, deps Deps, versionChanging bool) error {
	if !versionChanging {
		return nil
	}
	if cfg.NoWaitIdle {
		if deps.Output != nil {
			_, _ = fmt.Fprintln(deps.Output, "WARNING: --no-wait-idle: running Workflows of the previous version will fail on their next step.")
		}
		return nil
	}
	return waitForUpgradeIdle(ctx, cfg, deps)
}

// waitForUpgradeIdle reads native Workflows using the selected Kubernetes
// credentials. It makes no changes and does not stop incoming submissions.
func waitForUpgradeIdle(ctx context.Context, cfg UpgradeConfig, deps Deps) error {
	limit := cfg.WaitTimeout
	if limit == 0 {
		limit = DefaultUpgradeWaitTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	if deps.RestConfig == nil {
		return fmt.Errorf("observe active Workflows: Kubernetes configuration missing; upgrade not attempted")
	}
	client, err := dynamic.NewForConfig(deps.RestConfig)
	if err != nil {
		return fmt.Errorf("observe active Workflows; upgrade not attempted: %w", err)
	}
	namespace, err := releasePipelineNamespace(ctx, deps, cfg.Namespace, cfg.deployedRevision)
	if err != nil {
		return fmt.Errorf("observe active Workflows; upgrade not attempted: %w", err)
	}
	if namespace == "" {
		namespace = DefaultArgoNamespace
	}
	workflows := client.Resource(schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "workflows"}).Namespace(namespace)
	output := deps.Output
	if output == nil {
		output = io.Discard
	}
	terminal := false
	if file, ok := output.(*os.File); ok {
		terminal = term.IsTerminal(int(file.Fd()))
	}
	if terminal {
		defer func() { _, _ = fmt.Fprintln(output) }()
	}
	deadline, _ := ctx.Deadline()
	var active []string
	for {
		if err := ctx.Err(); err != nil {
			return upgradeIdleRefusal(active, err)
		}
		observed, err := activeUpgradeWorkflows(ctx, workflows)
		if err != nil {
			return upgradeIdleRefusal(active, err)
		}
		active = observed
		if err := printUpgradeIdleProgress(output, terminal, len(active), max(0, time.Until(deadline))); err != nil {
			return upgradeIdleRefusal(active, err)
		}
		// Never proceed after the deadline, even if the final API read says idle.
		if err := ctx.Err(); err != nil {
			return upgradeIdleRefusal(active, err)
		}
		if len(active) == 0 {
			return nil
		}
		timer := time.NewTimer(pollInterval(deps))
		select {
		case <-ctx.Done():
			timer.Stop()
			return upgradeIdleRefusal(active, ctx.Err())
		case <-timer.C:
		}
	}
}

func activeUpgradeWorkflows(ctx context.Context, workflows dynamic.ResourceInterface) ([]string, error) {
	var active []string
	options := metav1.ListOptions{LabelSelector: "oberth.ci/run", Limit: 500}
	for {
		list, err := workflows.List(ctx, options)
		if err != nil {
			return active, fmt.Errorf("read active Workflows from Kubernetes: %w", err)
		}
		for _, workflow := range list.Items {
			phase, _, err := unstructured.NestedString(workflow.Object, "status", "phase")
			if err != nil {
				return active, fmt.Errorf("read Workflow %s status: %w", workflow.GetName(), err)
			}
			switch phase {
			case "Succeeded", "Failed", "Error":
				continue
			}
			active = append(active, fmt.Sprintf("%s/%s (run %s, phase %s)", workflow.GetNamespace(), workflow.GetName(), workflow.GetLabels()["oberth.ci/run"], phase))
		}
		options.Continue = list.GetContinue()
		if options.Continue == "" {
			slices.Sort(active)
			return active, nil
		}
	}
}

func printUpgradeIdleProgress(output io.Writer, terminal bool, active int, remaining time.Duration) error {
	prefix, suffix := "", "\n"
	if terminal {
		prefix, suffix = "\r\033[2K", ""
	}
	_, err := fmt.Fprintf(output, "%sWaiting before upgrade: %d active Workflows; time left %s%s", prefix, active, remaining.Round(time.Second), suffix)
	return err
}

func upgradeIdleRefusal(active []string, cause error) error {
	runs := "unknown (no complete Workflow observation)"
	if len(active) > 0 {
		runs = strings.Join(active, ", ")
	}
	return fmt.Errorf("wait for idle refused; upgrade not attempted; still-running runs: %s: %w", runs, cause)
}
