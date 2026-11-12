package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/oberthci/oberth/internal/installer"
)

const clientPortsUsage = `Usage: oberth client ports --plan-file <public plan JSON> [flags]
Probe the advertised client ports before install and propose host port mappings.
Reads the selected plan's node identities; local Docker inspection is optional.
Explicit client ports override discovered mappings. Prints advisory JSON only;
TCP success does not establish server readiness or TLS trust. Nothing is installed.`

var clientPortsProbeDeps = installer.DefaultClientPortProbeDeps

func runClientPorts(ctx context.Context, arguments []string, output io.Writer) error {
	var planFile, endpointAddress string
	var cfg installer.Config
	flags := flag.NewFlagSet("client ports", flag.ContinueOnError)
	flags.StringVar(&endpointAddress, "endpoint-address", "", "detected node IP or load-balancer address to name and re-probe")
	flags.StringVar(&planFile, "plan-file", "", "public JSON from install --plan --json")
	flags.StringVar(&cfg.ClientHost, "client-host", "", "advertised host (default: plan review client_host)")
	flags.StringVar(&cfg.ClientHTTPSPort, "client-https-port", "", "explicit host HTTPS port; overrides discovery")
	flags.StringVar(&cfg.ClientSSHPort, "client-ssh-port", "", "explicit host SSH port; overrides discovery")
	flags.StringVar(&cfg.ServiceType, "service-type", "NodePort", "planned Service type: NodePort, ClusterIP or LoadBalancer")
	positional, help, err := parseOperatorFlags(flags, arguments)
	if err != nil {
		return err
	}
	if help {
		return commandFlagHelp(flags, clientPortsUsage)
	}
	if len(positional) != 0 || planFile == "" {
		return fmt.Errorf("%w: client ports requires --plan-file and no positional arguments", errUsage)
	}
	f, err := os.Open(planFile) //nolint:gosec // G304: bounded public plan explicitly selected by the operator.
	if err != nil {
		return errors.New("cannot open public cluster plan")
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil || len(raw) > 1<<20 {
		return errors.New("public cluster plan is unreadable or exceeds 1 MiB")
	}
	var plan installer.InstallPlan
	if json.Unmarshal(raw, &plan) != nil {
		return errors.New("invalid public cluster plan JSON")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	deps := clientPortsProbeDeps()
	deps.EndpointAddress = endpointAddress
	result, err := installer.ProbeClientPorts(ctx, plan, cfg, deps)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}
