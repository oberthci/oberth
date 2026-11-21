package argojob_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/argojob/argojobtest"
	"github.com/oberthci/oberth/internal/nativebinding"
	rbac "k8s.io/api/rbac/v1"
	"k8s.io/client-go/transport"
)

func TestProjectedTokenSourceRereadsAfterRotation(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("synthetic-first"), 0600); err != nil {
		t.Fatal(err)
	}
	source := transport.NewCachedFileTokenSource(path)
	first, err := source.Token()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("synthetic-rotated"), 0600); err != nil {
		t.Fatal(err)
	}
	source.ResetTokenOlderThan(time.Unix(1<<40, 0))
	next, err := source.Token()
	if err != nil {
		t.Fatal(err)
	}
	if first.AccessToken == next.AccessToken || next.AccessToken != "synthetic-rotated" {
		t.Fatal("rotation was not reread")
	}
}
func TestTaskResultRoleHasOnlyNativeExecutorPermissions(t *testing.T) {
	t.Parallel()
	role, binding := argojobtest.TaskResultAccess("ci")
	want := []rbac.PolicyRule{{APIGroups: []string{"argoproj.io"}, Resources: []string{"workflowtaskresults"}, Verbs: []string{"create", "patch"}}}
	if role.Namespace != "ci" || !reflect.DeepEqual(role.Rules, want) {
		t.Fatal("excess native authority")
	}
	wantSubjects := []rbac.Subject{{Kind: "Group", APIGroup: "rbac.authorization.k8s.io", Name: "system:serviceaccounts:ci"}}
	wantRef := rbac.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: role.Name}
	if binding.Namespace != role.Namespace || !reflect.DeepEqual(binding.Subjects, wantSubjects) || binding.RoleRef != wantRef {
		t.Fatal("task-result authority is not confined to pipeline accounts in their namespace")
	}
}
func TestWorkflowNamesPrefixFree(t *testing.T) {
	t.Parallel()
	names := []string{}
	for _, id := range []string{"a", "ab", "run-1", strings.Repeat("z", 1000)} {
		name := nativebinding.WorkflowName(id)
		if len(name) != 36 {
			t.Fatal(name)
		}
		names = append(names, name)
	}
	for _, id := range []int64{1, 36, 9223372036854775807} {
		for _, entry := range []string{"nightly", "daily"} {
			parent, err := nativebinding.CronName(id, entry)
			if err != nil {
				t.Fatal(err)
			}
			for _, stamp := range []string{"1", "12", "123"} {
				names = append(names, parent+"-"+stamp)
			}
		}
	}
	for i, a := range names {
		for j, b := range names {
			if i != j && strings.HasPrefix(a, b+"-") {
				t.Fatal("Workflow names overlap")
			}
		}
	}
}
