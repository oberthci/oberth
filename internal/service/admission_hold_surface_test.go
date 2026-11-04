package service

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func TestAdmissionHoldRESTUsesAdminMutationAndValidation(t *testing.T) {
	f := newControlFixture(t)
	control := f.api(t)
	for _, test := range []struct {
		actor       api.Actor
		token, body string
		code        int
	}{
		{api.Actor{Identity: "admin", Admin: true}, "", `{"on":true,"reason":"x"}`, 401},
		{api.Actor{Identity: "reader"}, "reader", `{"on":true,"reason":"x"}`, 403},
		{api.Actor{Identity: "scoped", Admin: true, Scope: []string{"codeberg"}}, "scoped", `{"on":true,"reason":"x"}`, 403},
		{api.Actor{Identity: "admin", Admin: true}, "admin", `{"reason":"missing on"}`, 400},
		{api.Actor{Identity: "admin", Admin: true}, "admin", `{"on":true,"reason":"x","duration":"13h"}`, 400},
		{api.Actor{Identity: "admin", Admin: true}, "admin", `{"on":true,"reason":"x","duration":"0s"}`, 400},
		{api.Actor{Identity: "admin", Admin: true}, "admin", `{"on":true,"reason":"x","allow":["branch"]}`, 400},
		{api.Actor{Identity: "admin", Admin: true}, "admin", `{"on":false,"reason":"x","allow":[]}`, 400},
		{api.Actor{Identity: "admin", Admin: true}, "admin", strings.Repeat(" ", (1<<20)+1), 413},
		{api.Actor{Identity: "admin", Admin: true}, "admin", `{"on":true,"reason":"release qualification","allow":["tag","release","promotion"],"duration":"12h"}`, 200},
		{api.Actor{Identity: "admin", Admin: true}, "admin", `{"on":false,"reason":"finished"}`, 200},
	} {
		server, err := api.New(scheduleSurfaceAuth{test.actor}, control, control, "test", api.WithErrorClassifier(func(err error) (int, string) {
			switch {
			case errors.Is(err, ErrForbidden):
				return 403, err.Error()
			case errors.Is(err, ErrInvalidInput), errors.Is(err, store.ErrInvalid):
				return 400, err.Error()
			default:
				return 500, err.Error()
			}
		}))
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/api/admission/hold", strings.NewReader(test.body))
		if test.token != "" {
			request.Header.Set("Authorization", "Bearer "+test.token)
		}
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != test.code {
			t.Fatalf("actor=%s body=%.100s: HTTP %d %s", test.actor.Identity, test.body, response.Code, response.Body)
		}
		if test.code == 200 {
			var got model.AdmissionHold
			if err = json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.On != f.scheduler.AdmissionHold().On || got.Reason != f.scheduler.AdmissionHold().Reason {
				t.Fatal("REST and gate disagree", got)
			}
		}
	}
	audit, err := f.store.LatestAuditActionByType(t.Context(), "admission.hold", "admission")
	if err != nil || audit.Actor != "admin" || !strings.Contains(audit.Details, "finished") {
		t.Fatal(audit, err)
	}
}
