package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	oberth "github.com/oberthci/oberth"
	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/pkg/periapsis"
)

// The secretstore command group splits secret-store work by where the
// authority for it lives:
//
//   - `setup` guidance and the embedded setup script belong NEXT TO the store,
//     where the administrator's own CLI session already holds admin authority.
//     This binary has no code path that accepts a store admin token.
//   - `verify` belongs IN THIS POD, because the thing under test is the
//     workload identity itself: it logs in with the pod's ServiceAccount token
//     through the production fetch code path and proves the whole trust chain
//     (auth mount, role binding, TokenReview, read policy, TLS) end to end
//     without any credential beyond the identity the server already has.
const defaultServeProcCmdline = "/proc/1/cmdline"

const maximumServeCmdlineBytes = 1 << 20

func runSecretStore(ctx context.Context, arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return fmt.Errorf("%w: secretstore setup|verify", errUsage)
	}
	switch arguments[0] {
	case "setup":
		return runSecretStoreSetup(arguments[1:], output)
	case "verify":
		return runSecretStoreVerify(ctx, arguments[1:], output, defaultServeProcCmdline)
	default:
		return fmt.Errorf("%w: unknown secretstore command %q", errUsage, arguments[0])
	}
}

func runSecretStoreSetup(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("secretstore setup", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	printScript := flags.Bool("print-script", false, "write the embedded setup script to stdout")
	if err := flags.Parse(arguments); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: secretstore setup accepts flags only", errUsage)
	}
	if *printScript {
		_, err := output.Write(oberth.SetupSecretStoreScript)
		return err
	}
	digest := sha256.Sum256(oberth.SetupSecretStoreScript)
	_, err := fmt.Fprintf(output, `Store-side setup runs NEXT TO your OpenBao (or Vault), never in this pod:
this binary deliberately has no code path that accepts a store admin token.

1. Extract the setup script from this signed image (or use the identical
   scripts/setup-secretstore.sh from the Oberth repository):

     kubectl exec -n oberth deploy/oberth -- \
       oberth secretstore setup --print-script > setup-secretstore.sh
     sha256sum setup-secretstore.sh   # %s

2. Inspect it, then run it on the machine where your bao/vault CLI is already
   authenticated. It enables Kubernetes auth, writes a minimal read-only
   policy, binds a role to the Oberth ServiceAccount, and prints the exact
   helm values for step 3.

3. helm upgrade --install with the printed secretstore.* values.

4. Prove the trust chain end to end from inside this pod:

     kubectl exec -n oberth deploy/oberth -- oberth secretstore verify
`, hex.EncodeToString(digest[:]))
	return err
}

// secretStoreVerifyConfig carries exactly the serve-side secret store settings
// the verifier needs; every field mirrors one --secretstore-* serve flag.
type secretStoreVerifyConfig struct {
	address      string
	authMount    string
	role         string
	caCertPath   string
	saTokenPath  string
	insecureHTTP bool
	paths        []string
}

func runSecretStoreVerify(ctx context.Context, arguments []string, output io.Writer, procCmdline string) error {
	flags := flag.NewFlagSet("secretstore verify", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var explicit secretStoreVerifyConfig
	flags.StringVar(&explicit.address, "address", "", "OpenBao API base URL (default: discovered from the running serve process)")
	flags.StringVar(&explicit.authMount, "k8s-auth-mount", "", "OpenBao Kubernetes auth mount path")
	flags.StringVar(&explicit.role, "role", "", "OpenBao Kubernetes auth role")
	flags.StringVar(&explicit.caCertPath, "ca-cert", "", "optional PEM file pinning the OpenBao TLS trust anchors")
	flags.StringVar(&explicit.saTokenPath, "sa-token", "", "optional ServiceAccount token path override")
	flags.BoolVar(&explicit.insecureHTTP, "insecure-http", false, "DEVELOPMENT ONLY: allow a plain-HTTP OpenBao address")
	timeout := flags.Duration("timeout", 45*time.Second, "overall verification deadline")
	if err := flags.Parse(arguments); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	options := explicit
	if options.address == "" {
		if options.role != "" || options.authMount != "" || options.caCertPath != "" || options.saTokenPath != "" || options.insecureHTTP {
			return fmt.Errorf("%w: explicit secretstore verify flags require --address", errUsage)
		}
		discovered, err := secretStoreConfigFromServeCmdline(procCmdline)
		if err != nil {
			return fmt.Errorf("discover the running server's secret store configuration: %w (run inside the Oberth pod, or pass --address and --role explicitly)", err)
		}
		options = discovered
	}
	if options.address == "" {
		return errors.New("this server has no secret store configured: serve runs without --secretstore-address; set secretstore.enabled=true in the chart first")
	}
	if strings.TrimSpace(options.role) == "" {
		return fmt.Errorf("%w: --address requires --role", errUsage)
	}
	// Positional arguments override the discovered allowlist so a candidate
	// path can be proven BEFORE an administrator allowlists it in the chart.
	paths := flags.Args()
	if len(paths) == 0 {
		paths = options.paths
	}
	if len(paths) == 0 {
		return errors.New("nothing to verify: the server allowlists no --secretstore-path values; pass one or more KV API paths (e.g. oberth/data/r2-upload) as arguments")
	}
	for _, path := range paths {
		if err := periapsis.ValidateSecretStorePath(path); err != nil {
			return err
		}
	}
	var caPEM []byte
	if options.caCertPath != "" {
		body, err := readBoundedFile(options.caCertPath, 4<<20)
		if err != nil {
			return fmt.Errorf("read secret store CA certificate: %w", err)
		}
		caPEM = body
	}
	client, err := secretstore.New(secretstore.Config{
		Address:                 options.address,
		AllowInsecureHTTP:       options.insecureHTTP,
		AuthMountPath:           options.authMount,
		Role:                    options.role,
		CACertPEM:               caPEM,
		ServiceAccountTokenPath: options.saTokenPath,
		Timeout:                 *timeout,
	})
	if err != nil {
		return fmt.Errorf("configure secret store verification: %w", err)
	}
	mount := options.authMount
	if mount == "" {
		mount = secretstore.DefaultAuthMountPath
	}
	if _, err := fmt.Fprintf(output, "secret store verify: address=%s mount=%s role=%s paths=%d\n",
		options.address, mount, options.role, len(paths)); err != nil {
		return err
	}
	deadlineCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	fetched, err := client.FetchKV(deadlineCtx, paths)
	if err != nil {
		_, _ = fmt.Fprint(output, secretStoreVerifyHints)
		return fmt.Errorf("secret store verify failed: %w", err)
	}
	// The fetch pulled real secret values through the production code path;
	// the verifier reports only key counts and wipes every value before it
	// returns. Values are never printed, logged, or written anywhere.
	defer zeroFetchedSecrets(fetched)
	for _, path := range paths {
		if _, err := fmt.Fprintf(output, "  ok %s (%d keys)\n", path, len(fetched[path])); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintln(output, "secret store verify: OK — ServiceAccount login, TokenReview, read policy, and TLS all verified")
	return err
}

const secretStoreVerifyHints = `common causes:
  - role or ServiceAccount binding missing on the store side
      -> re-run scripts/setup-secretstore.sh next to the store
  - TokenReview rejected for an out-of-cluster store
      -> the chart's system:auth-delegator binding (secretstore.createAuthDelegatorBinding) must exist
  - the store cannot reach this cluster's API server
      -> check the kubernetes_host that setup wrote (setup-secretstore.sh --kubernetes-host)
  - path missing, deleted, or outside the store-side read policy
      -> check the KV API path (KV v2 paths include /data/) and the policy printed by setup
`

func zeroFetchedSecrets(fetched map[string]map[string][]byte) {
	for _, values := range fetched {
		for _, value := range values {
			for index := range value {
				value[index] = 0
			}
		}
	}
}

// secretStoreConfigFromServeCmdline reads the live serve process's own command
// line (PID 1 in the Oberth pod) and extracts exactly the --secretstore-*
// flags, so verification always tests the configuration the server actually
// runs with — not a hand-retyped copy that could hide a drift.
//
// The scanner is deliberately tolerant of every spelling the Go flag package
// accepts (-flag=value, --flag=value, -flag value, --flag value) and ignores
// all non-secretstore flags; TestSecretStoreCmdlineMatchesServeParsing pins it
// against parseServeOptions so the two can never diverge silently.
func secretStoreConfigFromServeCmdline(path string) (secretStoreVerifyConfig, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- fixed /proc/1/cmdline, overridden only by tests.
	if err != nil {
		return secretStoreVerifyConfig{}, fmt.Errorf("read serve process command line: %w", err)
	}
	if len(raw) > maximumServeCmdlineBytes {
		return secretStoreVerifyConfig{}, errors.New("serve process command line exceeds the size bound")
	}
	fields := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	if len(fields) < 2 || !strings.HasSuffix(fields[0], "oberth") || fields[1] != "serve" {
		return secretStoreVerifyConfig{}, errors.New("PID 1 is not an oberth serve process")
	}
	var config secretStoreVerifyConfig
	arguments := fields[2:]
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if len(argument) < 2 || argument[0] != '-' {
			continue
		}
		name := strings.TrimPrefix(strings.TrimPrefix(argument, "-"), "-")
		value := ""
		hasValue := false
		if equals := strings.IndexByte(name, '='); equals >= 0 {
			value = name[equals+1:]
			name = name[:equals]
			hasValue = true
		}
		consumeValue := func() error {
			if hasValue {
				return nil
			}
			index++
			if index >= len(arguments) {
				return fmt.Errorf("serve flag --%s has no value", name)
			}
			value = arguments[index]
			return nil
		}
		switch name {
		case "secretstore-address":
			if err := consumeValue(); err != nil {
				return secretStoreVerifyConfig{}, err
			}
			config.address = value
		case "secretstore-k8s-auth-mount":
			if err := consumeValue(); err != nil {
				return secretStoreVerifyConfig{}, err
			}
			config.authMount = value
		case "secretstore-role":
			if err := consumeValue(); err != nil {
				return secretStoreVerifyConfig{}, err
			}
			config.role = value
		case "secretstore-ca-cert":
			if err := consumeValue(); err != nil {
				return secretStoreVerifyConfig{}, err
			}
			config.caCertPath = value
		case "secretstore-sa-token":
			if err := consumeValue(); err != nil {
				return secretStoreVerifyConfig{}, err
			}
			config.saTokenPath = value
		case "secretstore-path":
			if err := consumeValue(); err != nil {
				return secretStoreVerifyConfig{}, err
			}
			config.paths = append(config.paths, value)
		case "secretstore-insecure-http":
			// The Go flag package accepts booleans only as -flag or -flag=value.
			enabled := true
			if hasValue {
				parsed, parseErr := strconv.ParseBool(value)
				if parseErr != nil {
					return secretStoreVerifyConfig{}, fmt.Errorf("serve flag --secretstore-insecure-http has invalid value %q", value)
				}
				enabled = parsed
			}
			config.insecureHTTP = enabled
		}
	}
	return config, nil
}
