package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strconv"

	"github.com/oberthci/oberth/internal/measured"
)

func runRunResources(ctx context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("run resources", flag.ContinueOnError)
	flags.SetOutput(output)
	repo := flags.String("repo", "", "registered repository (optional with --run)")
	run := flags.String("run", "", "exact run ID or unique prefix")
	ref := flags.String("ref", "", "exact ref; default includes default branch and its promotion runs; any includes all refs")
	burn := flags.String("burn", "", "optional burn")
	step := flags.String("step", "", "optional template name")
	last := flags.Int("last", measured.DefaultRuns, "last N runs, maximum 100")
	includeFailed := flags.Bool("include-failed", false, "include failed and incomplete runs")
	format := flags.String("format", "json", "json or yaml")
	asYAML := flags.Bool("yaml", false, "YAML report with paste-ready resources")
	asJSON := flags.Bool("json", false, "JSON report (default)")
	if e := flags.Parse(arguments); e != nil {
		return e
	}
	if flags.NArg() == 1 && *repo == "" {
		*repo = flags.Arg(0)
	} else if flags.NArg() != 0 {
		return fmt.Errorf("%w: run resources [--repo repo | --run id] [--yaml]", errUsage)
	}
	if *asYAML {
		*format = "yaml"
	}
	if *asJSON && *asYAML {
		return fmt.Errorf("%w: choose json or yaml", errUsage)
	}
	if *format != "json" && *format != "yaml" || *last < 1 || *last > measured.MaxRuns || *repo == "" && *run == "" {
		return fmt.Errorf("%w: run resources requires repo or run; last 1..100; format json or yaml", errUsage)
	}
	api, e := remoteClient(ctx)
	if e != nil {
		return e
	}
	query := map[string]string{"repo": *repo, "run": *run, "ref": *ref, "burn": *burn, "step": *step, "last": strconv.Itoa(*last), "include_failed": strconv.FormatBool(*includeFailed)}
	var report measured.Report
	if e = api.Get(ctx, "/api/run-resources", query, &report); e != nil {
		return e
	}
	if *format == "yaml" {
		_, e = io.WriteString(output, measured.YAML(report))
		return e
	}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}
