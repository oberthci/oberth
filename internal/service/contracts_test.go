package service_test

import (
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/service/contracttest"
)

func TestEffectAndAuthorityConsumerContracts(t *testing.T) {
	t.Parallel()
	t.Run("authority", func(t *testing.T) { f := setup(t); contracttest.AuthorityStore(t, f.store, caller()) })
	t.Run("catalog", func(t *testing.T) { f := setup(t); contracttest.Catalog(t, f.store, caller(), repository) })
	t.Run("effects", func(t *testing.T) { f := setup(t); r := f.seed("running"); contracttest.Effects(t, f.exec, f.logs, r) })
	t.Run("starter", func(t *testing.T) {
		f := setup(t)
		contracttest.Starter(t, f.starter, service.StartRequest{Request: model.CIRequest{Repo: repository}})
	})
}
