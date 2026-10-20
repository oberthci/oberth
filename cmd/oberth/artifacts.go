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
	if err := flags.Parse(permuteFlagsFirst(arguments)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	rest := flags.Args()
	if len(rest) == 0 || len(rest) > 2 {
		return fmt.Errorf("%w: artifacts [--output FILE] <run-id> [name]", errUsage)
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
	if config := client.FromEnv(); config.Configured() && !localForced {
		return remoteArtifacts(ctx, config, rest, outputPath, output)
	}
	if localForced && client.FromEnv().Configured() {
		fmt.Fprintf(os.Stderr, "note: --data-root overrides OBERTH_BASE_URL; reading local store\n")
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
