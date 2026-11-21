package issuer

import (
	"io"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestLoadValidAuthorityRefusesMissingDependencies(t *testing.T) {
	_, files := authorityFor(t)
	for _, tc := range []struct {
		name   string
		random io.Reader
		now    func() time.Time
	}{{"entropy", nil, instant}, {"clock", entropy(), nil}} {
		t.Run(tc.name, func(t *testing.T) {
			authority, err := Load(files["ca.crt"], files["ca.key"], tc.random, tc.now)
			if authority != nil || err == nil {
				t.Fatal("incomplete authority loaded")
			}
			problem := publicerr.Classify(err)
			if problem.Code() != publicerr.InvalidInput || problem.Reason() != "Authority dependencies are missing." || problem.Remedy() != "Supply entropy and a clock." {
				t.Fatal("dependency failure was misdiagnosed", problem)
			}
		})
	}
}
