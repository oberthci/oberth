package main

import (
	"errors"
	"strings"

	"github.com/oberthci/oberth/internal/publicerr"
)

func repoAddTrustFailure(err error, address, known, upstream string) error {
	var problem *publicerr.Error
	if !errors.As(err, &problem) || problem.ID != forgeTrustID {
		return err
	}
	problem.Context = append(problem.Context, "Retry: "+publicerr.RepoAddTrustCommand(address, known, upstream, false), "Changed key only: "+publicerr.RepoAddTrustCommand(address, known, upstream, true))
	return err
}

func shellArgument(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
