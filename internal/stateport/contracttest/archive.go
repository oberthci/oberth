package contracttest

import (
	"bytes"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func badBackups(t *testing.T, create factory) {
	t.Helper()
	for _, kind := range []string{"empty", "flip", "truncate", "trailing"} {
		t.Run(kind, func(t *testing.T) {
			f := prepare(t, create, 0)
			body, _ := f.snapshot(t)
			bad := bytes.Clone(body)
			switch kind {
			case "empty":
				bad = nil
			case "flip":
				bad[len(bad)/2] ^= 1
			case "truncate":
				bad = bad[:len(bad)/2]
			case "trailing":
				bad = append(bad, []byte("trailing data")...)
			}
			assertBadBackup(t, f, bad, body)
		})
	}
}

func assertBadBackup(t *testing.T, f Fixture, bad, valid []byte) {
	t.Helper()
	_, err := f.Port.Inspect(t.Context(), f.Options, bytes.NewReader(bad))
	assertCode(t, err, publicerr.SchemaRefused)
	request := f.restoreRequest(t)
	_, err = f.Port.Restore(t.Context(), request, bytes.NewReader(bad))
	assertCode(t, err, publicerr.SchemaRefused)
	f.assertDestination(t, request.Destination, false)
	if _, err := f.Port.Restore(t.Context(), request, bytes.NewReader(valid)); err != nil {
		t.Fatal("failed restore changed destination", err)
	}
	f.assertDestination(t, request.Destination, true)
}

func foreignArchives(t *testing.T, create factory) {
	t.Helper()
	for index, name := range []string{"schema_identity", "schema_number", "audit_domain"} {
		t.Run(name, func(t *testing.T) {
			base, foreign := prepare(t, create, 0), prepare(t, create, index+1)
			checkForeignFixture(t, base, foreign, index+1)
			valid, _ := base.snapshot(t)
			incoming, _ := foreign.snapshot(t)
			assertBadBackup(t, base, incoming, valid)
		})
	}
}

func checkForeignFixture(t *testing.T, base, foreign Fixture, index int) {
	t.Helper()
	adjusted := foreign.Options
	switch index {
	case 1:
		adjusted.Protocol.SchemaIdentity = base.Options.Protocol.SchemaIdentity
	case 2:
		adjusted.Schema = base.Options.Schema
	case 3:
		adjusted.Protocol.AuditDomain = base.Options.Protocol.AuditDomain
	}
	if adjusted != base.Options || foreign.Options == base.Options {
		t.Fatal("foreign fixture must differ in exactly its selected binding")
	}
	if foreign.WantContent != base.WantContent || foreign.WantHead != base.WantHead {
		t.Fatal("foreign fixture must preserve known content and head")
	}
}
