package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

const admissionHoldUsage = "Usage: oberth admission hold on|off --reason <reason> [--allow tag,release,promotion] [--duration 2h] [--json]\n" +
	"         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]\nDefault allow: tag,release. Default duration: 2h; maximum: 12h. --allow '' holds all new admission."

func runAdmissionHold(ctx context.Context, args []string, output io.Writer) error {
	if wantsHelp(args) {
		_, err := fmt.Fprintln(output, admissionHoldUsage)
		return err
	}
	flags := flag.NewFlagSet("admission hold", flag.ContinueOnError)
	reason := flags.String("reason", "", "reason for the hold change")
	allow := flags.String("allow", "", "comma-separated allowed classes")
	duration := flags.String("duration", "", "hold duration, default 2h, maximum 12h")
	asJSON := flags.Bool("json", false, "emit JSON")
	transport := mcpTransportFlags(flags)
	positional, _, err := parseOperatorFlags(flags, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 || (positional[0] != "on" && positional[0] != "off") || strings.TrimSpace(*reason) == "" {
		return fmt.Errorf("%w: %s", errUsage, admissionHoldUsage)
	}
	arguments := map[string]any{"on": positional[0] == "on", "reason": *reason}
	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "allow":
			if *allow == "" {
				arguments["allow"] = []string{}
			} else {
				arguments["allow"] = strings.Split(*allow, ",")
			}
		case "duration":
			arguments["duration"] = *duration
		}
	})
	config, err := parseMCPFlags("admission hold", transport(), 0)
	if err != nil {
		return err
	}
	raw, err := (remoteRunCanceller{config: config}).call(ctx, "admission_hold", arguments)
	if err != nil {
		return err
	}
	var hold model.AdmissionHold
	if *asJSON || json.Unmarshal(raw, &hold) != nil {
		_, err = fmt.Fprintf(output, "%s\n", strings.TrimSpace(string(raw)))
		return err
	}
	if hold.On {
		_, err = fmt.Fprintln(output, hold.Message())
	} else {
		_, err = fmt.Fprintln(output, "admission hold off: "+hold.Reason)
	}
	return err
}
