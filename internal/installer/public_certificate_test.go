package installer

import (
	"context"
	"errors"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

func TestRuntimeCertificateUsesAuthenticatedAPIOnly(t *testing.T) {
	for _, allowed := range []bool{true, false} {
		t.Run(map[bool]string{true: "allowed", false: "refused"}[allowed], func(t *testing.T) {
			kube := fake.NewClientset()
			calls := 0
			denied := errors.New("admin identity required")
			deps := Deps{KubeClient: kube, Admin: idleAdminFunc(func(_ context.Context, action string, args, into any) error {
				calls++
				if action != "public-identity" || args.(map[string]string)["name"] != "server-certificate" {
					t.Fatal(action, args)
				}
				if !allowed {
					return denied
				}
				return fixtureDecode(map[string]string{"content": "public leaf"}, into)
			}), RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) {
				t.Fatal("server exec attempted")
				return nil, nil
			}}
			raw, err := readServerPublicCertificate(t.Context(), Config{}, deps, false)
			if calls != 1 || len(kube.Actions()) != 0 {
				t.Fatal(calls, kube.Actions())
			}
			if allowed {
				if err != nil || string(raw) != "public leaf" {
					t.Fatal(err)
				}
			} else if !errors.Is(err, denied) {
				t.Fatal(err)
			}
		})
	}
}
