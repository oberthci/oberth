package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/publicerr"
)

func repoAddTrustFailure(err error, address, known, upstream string) error {
	var problem *publicerr.Error
	if !errors.As(err, &problem) || problem.ID != forgeTrustID {
		return err
	}
	retry := fmt.Sprintf("oberth repo add %s --known-hosts %s --expected-host-fingerprint VERIFIED_FINGERPRINT", shellArgument(address), shellArgument(known))
	retry += " --upstream " + shellArgument(upstream)
	problem.Context = append(problem.Context, "Retry: "+retry, "Changed key only: "+retry+" --repin")
	return err
}

func shellArgument(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
