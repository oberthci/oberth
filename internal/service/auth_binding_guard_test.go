package service

import (
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

func TestAuthenticatorRefusesIncompleteUplinkBinding(t *testing.T) {
	for _, uplink := range []model.Uplink{
		{Identity: " \t", Fingerprint: "SHA256:fixture", Admin: true},
		{Identity: "operator", Fingerprint: " \n", Admin: true},
	} {
		authenticator, err := NewAuthenticator(stubUplinkAuthenticator{value: model.AuthenticatedUplink{Uplink: uplink}})
		if err != nil {
			t.Fatal(err)
		}
		actor, err := authenticator.Authenticate(t.Context(), "fixture")
		if err == nil || !reflect.DeepEqual(actor, api.Actor{}) {
			t.Fatalf("incomplete uplink produced an actor: %+v, %v", actor, err)
		}
	}
}
