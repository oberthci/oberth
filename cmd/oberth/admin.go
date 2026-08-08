package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/auth"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

const administrativeActor = "admin@localhost"

type upstreamDependencies struct {
	input            io.Reader
	kubernetesClient func() (kubernetes.Interface, error)
	scanHostKeys     app.SSHHostKeyScanner
	probe            app.SSHCapabilityProbe
	mutationGate     func(context.Context, string, string) error
}

type uplinkDependencies struct {
	mutationGate func(context.Context, string, string) error
}

func runUpstream(ctx context.Context, arguments []string, output io.Writer) error {
	return runUpstreamWithDependencies(ctx, arguments, output, upstreamDependencies{
		input: os.Stdin,
		kubernetesClient: func() (kubernetes.Interface, error) {
			configuration, err := rest.InClusterConfig()
			if err != nil {
				return nil, err
			}
			configuration.Timeout = 10 * time.Second
			return kubernetes.NewForConfig(configuration)
		},
		scanHostKeys: app.ScanSSHHostKeys,
		probe:        app.ProbeSSHCapability,
		mutationGate: requestLiveAdminMutationGate,
	})
}

func runUpstreamWithDependencies(ctx context.Context, arguments []string, output io.Writer, dependencies upstreamDependencies) error {
	if len(arguments) == 0 || arguments[0] != "add" {
		return fmt.Errorf("%w: upstream add [flags] <name> <base-url>", errUsage)
	}
	flags := flag.NewFlagSet("upstream add", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	databasePath := flags.String("database", "/data/oberth.sqlite", "SQLite database path")
	upstreamKey := flags.String("upstream-key", "/etc/oberth/upstream-key/id_ed25519", "upstream SSH private key")
	knownHosts := flags.String("known-hosts", "/etc/oberth/known-hosts/known_hosts", "upstream known_hosts")
	namespace := flags.String("namespace", "oberth", "Kubernetes namespace")
	upstreamKeySecret := flags.String("upstream-key-secret", "oberth-upstream-key", "upstream SSH private-key Secret")
	knownHostsSecret := flags.String("known-hosts-secret", "oberth-known-hosts", "upstream known_hosts Secret")
	if err := flags.Parse(arguments[1:]); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 2 {
		return fmt.Errorf("%w: upstream add requires name and base URL", errUsage)
	}
	name, baseURL := flags.Arg(0), strings.TrimSuffix(flags.Arg(1), "/")
	if name == "" || name != strings.TrimSpace(name) || strings.ContainsAny(name, "\x00\r\n ") {
		return fmt.Errorf("%w: upstream name is invalid", errUsage)
	}
	kind, err := app.UpstreamKind(baseURL)
	if err != nil {
		return err
	}
	if kind == "ssh" {
		secretGate := func(ctx context.Context, operation string) error {
			if dependencies.mutationGate == nil {
				return errors.New("admin audit mutation gate is unavailable")
			}
			return dependencies.mutationGate(ctx, operation, *databasePath)
		}
		if _, err := (app.UpstreamSSHBootstrap{
			Input: dependencies.input, Output: output,
			KubernetesClient:  dependencies.kubernetesClient,
			ScanHostKeys:      dependencies.scanHostKeys,
			Probe:             dependencies.probe,
			Namespace:         *namespace,
			PrivateKeySecret:  *upstreamKeySecret,
			KnownHostsSecret:  *knownHostsSecret,
			PrivateKeyPath:    *upstreamKey,
			KnownHostsPath:    *knownHosts,
			PrivateKeyDataKey: filepath.Base(*upstreamKey),
			PublicKeyDataKey:  filepath.Base(*upstreamKey) + ".pub",
			KnownHostsDataKey: filepath.Base(*knownHosts),
			MutationGate:      secretGate,
		}).Ensure(ctx, baseURL); err != nil {
			return fmt.Errorf("upstream SSH identity is not ready: %w", err)
		}
	}
	if dependencies.mutationGate == nil {
		return errors.New("admin audit mutation gate is unavailable")
	}
	if err := dependencies.mutationGate(ctx, "upstream.database.open", *databasePath); err != nil {
		return err
	}
	database, err := store.OpenAdminClient(ctx, *databasePath, store.Options{})
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()
	if err := dependencies.mutationGate(ctx, "upstream.register", *databasePath); err != nil {
		return err
	}
	value, err := database.RegisterUpstream(ctx, administrativeActor, model.UpstreamSpec{Name: name, Kind: kind, BaseURL: baseURL})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "registered upstream %s (%s)\n", value.Name, value.Kind)
	return err
}

func runUplink(ctx context.Context, arguments []string, input io.Reader, output io.Writer) error {
	return runUplinkWithDependencies(ctx, arguments, input, output, uplinkDependencies{mutationGate: requestLiveAdminMutationGate})
}

func runUplinkWithDependencies(ctx context.Context, arguments []string, input io.Reader, output io.Writer, dependencies uplinkDependencies) error {
	if len(arguments) == 0 || arguments[0] != "add" {
		return fmt.Errorf("%w: uplink add [--database PATH] [--tls-cert PATH] <public-key-file|raw-key|-> <identity@host>", errUsage)
	}
	flags := flag.NewFlagSet("uplink add", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	databasePath := flags.String("database", "/data/oberth.sqlite", "SQLite database path")
	certificatePath := flags.String("tls-cert", "/etc/oberth/tls/tls.crt", "HTTPS certificate path")
	if err := flags.Parse(arguments[1:]); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 2 {
		return fmt.Errorf("%w: uplink add requires one public-key source and identity", errUsage)
	}
	identity := flags.Arg(1)
	if err := app.ValidateUplinkIdentity(identity); err != nil {
		return err
	}
	var fingerprint string
	var err error
	if flags.Arg(0) == "-" {
		fingerprint, err = app.AuthorizedKeyFingerprintReader(input)
	} else if _, statErr := os.Stat(flags.Arg(0)); statErr != nil {
		fingerprint, err = app.AuthorizedKeyFingerprintReader(strings.NewReader(flags.Arg(0)))
	} else {
		fingerprint, err = app.AuthorizedKeyFingerprint(flags.Arg(0))
	}
	if err != nil {
		return err
	}
	tlsFingerprint, err := app.TLSCertificateFingerprint(*certificatePath)
	if err != nil {
		return err
	}
	if dependencies.mutationGate == nil {
		return errors.New("admin audit mutation gate is unavailable")
	}
	gate := func(ctx context.Context, operation string) error {
		return dependencies.mutationGate(ctx, operation, *databasePath)
	}
	if err := gate(ctx, "uplink.database.open"); err != nil {
		return err
	}
	database, err := store.OpenAdminClient(ctx, *databasePath, store.Options{})
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()
	tokens := gatedAdminTokens{Store: database, Gate: gate}
	issuer := auth.Issuer{
		Tokens: tokens,
		Bind: func(ctx context.Context, pending model.TokenCredential) error {
			if err := gate(ctx, "uplink.register"); err != nil {
				return err
			}
			_, err := database.RegisterUplink(ctx, administrativeActor, model.UplinkSpec{
				Fingerprint: fingerprint, Identity: identity, TokenCredentialID: pending.ID,
			})
			return err
		},
	}
	if _, err := fmt.Fprintf(output, "TLS certificate fingerprint: %s\nUplink token for %s (shown once):\n", tlsFingerprint, identity); err != nil {
		return err
	}
	if _, err := issuer.Issue(ctx, identity, output); err != nil {
		return err
	}
	return nil
}

type gatedAdminTokens struct {
	*store.Store
	Gate func(context.Context, string) error
}

func (tokens gatedAdminTokens) CreateTokenCredential(ctx context.Context, spec model.TokenCredentialSpec) (model.TokenCredential, error) {
	if err := tokens.Gate(ctx, "uplink.token.create"); err != nil {
		return model.TokenCredential{}, err
	}
	return tokens.Store.CreateTokenCredential(ctx, spec)
}

func (tokens gatedAdminTokens) ActivateTokenCredential(ctx context.Context, id string) (model.TokenCredential, error) {
	if err := tokens.Gate(ctx, "uplink.token.activate"); err != nil {
		return model.TokenCredential{}, err
	}
	return tokens.Store.ActivateTokenCredential(ctx, id)
}

func (tokens gatedAdminTokens) TouchTokenCredential(ctx context.Context, id string) error {
	if err := tokens.Gate(ctx, "uplink.token.touch"); err != nil {
		return err
	}
	return tokens.Store.TouchTokenCredential(ctx, id)
}
