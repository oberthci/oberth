package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/oberthci/oberth/internal/artifacts"
	"github.com/oberthci/oberth/internal/client"
)

func runArtifacts(ctx context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("artifacts", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dataRoot := flags.String("data-root", "/data", "Oberth data root holding the artifact store")
	var outputPath string
	flags.StringVar(&outputPath, "output", "", "save the artifact to this file (written atomically, digest-verified)")
	flags.StringVar(&outputPath, "o", "", "shorthand for --output")
	transport := registerTransportFlags(flags)
	flags.Usage = func() {
		_, _ = fmt.Fprintln(flags.Output(), "Usage: oberth artifacts <run-id> [name] [--output FILE]\nLists a run's artifacts, or writes one artifact to stdout or --output.")
		flags.PrintDefaults()
	}
	if err := parseCommandFlags(flags, arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return usageError(err)
	}
	rest := flags.Args()
	if len(rest) == 0 || len(rest) > 2 {
		return fmt.Errorf("%w: artifacts [--output FILE] <run-id> [name]", errUsage)
	}
	if err := refuseVerbAsRunID("artifacts", rest[0], "<run-id> [name]: oberth artifacts <run-id> lists a run's artifacts"); err != nil {
		return err
	}
	if outputPath != "" && len(rest) != 2 {
		return fmt.Errorf("%w: --output needs an artifact name", errUsage)
	}
	// An explicit --data-root forces local mode even when OBERTH_BASE_URL
	// is set, so the flag is never silently ignored.
	localForced := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "data-root" {
			localForced = true
		}
	})
	// Without --data-root the server named by the transport flags, the
	// environment or the client files is read; only when none names one
	// does the command read the local store (in-pod).
	connection, resolveErr := client.ResolveConnection(*transport)
	remote := !errors.Is(resolveErr, client.ErrNotConfigured)
	if remote && !localForced {
		if resolveErr != nil {
			return resolveErr
		}
		return remoteArtifacts(ctx, connection.Config(), rest, outputPath, output)
	}
	if localForced && remote {
		fmt.Fprintf(os.Stderr, "note: --data-root overrides the configured server; reading local store\n")
	}
	reportMode("local store")
	store, err := artifacts.Open(*dataRoot + "/artifacts")
	if err != nil {
		return err
	}
	if len(rest) == 2 {
		copyLocal := func(destination io.Writer) error {
			file, openErr := store.OpenArtifact(rest[0], rest[1])
			if openErr != nil {
				return openErr
			}
			defer func() { _ = file.Close() }()
			_, copyErr := io.Copy(destination, file)
			return copyErr
		}
		if outputPath != "" {
			return saveArtifactFile(outputPath, copyLocal)
		}
		return copyLocal(output)
	}
	entries, err := store.List(rest[0])
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		_, err := fmt.Fprintf(output, "run %s kept no artifacts\n", rest[0])
		return err
	}
	if _, err := fmt.Fprintf(output, "%-12s  %-20s  %s\n", "SIZE", "MODIFIED", "NAME"); err != nil {
		return err
	}
	for _, entry := range entries {
		if _, err := fmt.Fprintf(output, "%-12d  %-20s  %s\n",
			entry.Size, entry.Modified.UTC().Format("2006-01-02 15:04:05"), entry.Name); err != nil {
			return err
		}
	}
	return nil
}
