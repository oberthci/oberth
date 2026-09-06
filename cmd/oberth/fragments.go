package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"sort"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func runFragments(ctx context.Context, arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return fmt.Errorf("%w: fragments list|show", errUsage)
	}
	switch arguments[0] {
	case "list":
		return runFragmentsList(ctx, arguments[1:], output)
	case "show":
		return runFragmentsShow(ctx, arguments[1:], output)
	default:
		return fmt.Errorf("%w: fragments list|show", errUsage)
	}
}

func fragmentFlags(name string, arguments []string, output io.Writer) (string, string, []string, error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	databasePath := flags.String("database", "/data/oberth.sqlite", "SQLite database path (in-pod)")
	dataRoot := flags.String("data-root", "/data", "Oberth data root holding the git cache")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return "", "", nil, nil
		}
		return "", "", nil, fmt.Errorf("%w: %w", errUsage, err)
	}
	return *databasePath, *dataRoot, flags.Args(), nil
}

func openFragmentCache(dataRoot string) (*gitcache.Cache, error) {
	return gitcache.New(gitcache.Config{
		Root:     filepath.Join(dataRoot, "git"),
		Upstream: func(string) (string, error) { return "", errors.New("fragment reads never contact an upstream") },
	})
}

func runFragmentsList(ctx context.Context, arguments []string, output io.Writer) error {
	databasePath, dataRoot, rest, err := fragmentFlags("fragments list", arguments, output)
	if err != nil || databasePath == "" {
		return err
	}
	if len(rest) != 0 {
		return fmt.Errorf("%w: fragments list accepts no arguments", errUsage)
	}
	database, err := store.OpenAdminClient(ctx, databasePath, store.Options{})
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()
	repositories, err := database.ListRepositories(ctx)
	if err != nil {
		return err
	}
	upstreams, err := database.ListUpstreams(ctx)
	if err != nil {
		return err
	}
	upstreamByID := make(map[int64]model.Upstream, len(upstreams))
	for _, upstream := range upstreams {
		upstreamByID[upstream.ID] = upstream
	}
	cache, err := openFragmentCache(dataRoot)
	if err != nil {
		return err
	}

	if _, err := fmt.Fprintf(output, "%-32s %s\n", "REPOSITORY", "VERSIONS"); err != nil {
		return err
	}
	for _, repository := range repositories {
		upstream, ok := upstreamByID[repository.UpstreamID]
		if !ok {
			continue
		}
		// Fully qualified so the read hits the org-qualified cache directory
		// (issue #264); parsed segments route the path without a qualifier.
		qualified := upstream.QualifiedRepo(repository.Name)
		refs, refErr := cache.SnapshotRefs(ctx, qualified)
		if refErr != nil {
			continue
		}
		var versions []string
		for ref := range refs {
			if tag, found := trimTagRef(ref); found {
				if _, err := cache.ReadBlob(ctx, qualified, refs[ref], argoworkflow.FragmentFile, argoworkflow.MaxSourceBytes); err == nil {
					versions = append(versions, tag)
				}
			}
		}
		if len(versions) == 0 {
			continue
		}
		sort.Strings(versions)
		if _, err := fmt.Fprintf(output, "%-32s %v\n", repository.Name, versions); err != nil {
			return err
		}
	}
	return nil
}

func trimTagRef(ref string) (string, bool) {
	const prefix = "refs/tags/"
	if len(ref) > len(prefix) && ref[:len(prefix)] == prefix {
		return ref[len(prefix):], true
	}
	return "", false
}

func runFragmentsShow(ctx context.Context, arguments []string, output io.Writer) error {
	databasePath, dataRoot, rest, err := fragmentFlags("fragments show", arguments, output)
	if err != nil || databasePath == "" {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("%w: fragments show <repository>@<version>", errUsage)
	}
	key, err := argoworkflow.ParseFragmentRef(rest[0])
	if err != nil {
		return err
	}
	database, err := store.OpenAdminClient(ctx, databasePath, store.Options{})
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()
	_, _, name, err := gitcache.ParseRepoPath(key.Repo)
	if err != nil {
		return err
	}
	registered, err := database.RepositoryRegistered(ctx, name)
	if err != nil {
		return err
	}
	if !registered {
		return fmt.Errorf("fragment %s names a repository this server does not host", key)
	}
	cache, err := openFragmentCache(dataRoot)
	if err != nil {
		return err
	}
	sha, err := cache.TagSHA(ctx, key.Repo, key.Version)
	if err != nil {
		return err
	}
	body, err := cache.ReadBlob(ctx, key.Repo, sha, argoworkflow.FragmentFile, argoworkflow.MaxSourceBytes)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "# %s resolves to %s\n", key, sha); err != nil {
		return err
	}
	_, err = output.Write(body)
	return err
}
