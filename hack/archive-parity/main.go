// archive-parity diagnoses a quiescent bare cache without writing to it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/archivecheck"
)

func main() {
	repo := flag.String("repo", "", "quiescent bare repository (never fetched or repacked)")
	commit := flag.String("commit", "", "full commit SHA")
	all := flag.Bool("all", false, "archive every commit reachable from every ref")
	inspect := flag.String("inspect", "", "also report the object/pack/delta chain for this path")
	timeout := flag.Duration("timeout", 30*time.Minute, "whole diagnostic liveness bound")
	flag.Parse()
	if *repo == "" || (*commit == "" && !*all) || (*commit != "" && *all) {
		flag.Usage()
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	checker, err := archivecheck.New(*repo)
	if err != nil {
		fail(err)
	}
	defer checker.Close()
	commits := []string{*commit}
	if *all {
		body, err := archivecheck.Git(ctx, *repo, "rev-list", "--all")
		if err != nil {
			fail(err)
		}
		commits = strings.Fields(string(body))
	}
	for i, sha := range commits {
		if err := checker.Check(ctx, sha); err != nil {
			fmt.Fprintf(os.Stderr, "FAIL commit=%s: %v\n", sha, err)
			var difference *archivecheck.Difference
			if errors.As(err, &difference) && difference.Path != "" {
				if e := archivecheck.Explain(ctx, *repo, sha, difference.Path, os.Stderr); e != nil {
					fmt.Fprintln(os.Stderr, e)
				}
			}
			checker.Close()
			os.Exit(1)
		}
		if *inspect != "" {
			if err := archivecheck.Explain(ctx, *repo, sha, *inspect, os.Stdout); err != nil {
				fail(err)
			}
		}
		if i%100 == 0 || i == len(commits)-1 {
			fmt.Printf("PASS commits=%d/%d last=%s\n", i+1, len(commits), sha)
		}
	}
}
func fail(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
