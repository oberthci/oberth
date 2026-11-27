package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// Every transaction entry has an observation owner. Identity mutations use the
// central audit classifier; use timestamps and schedule admission bookkeeping
// are internal, while enrollment changes installation authority.
func TestScopedChangeWriteInventory(t *testing.T) {
	t.Parallel()
	public := strings.Fields(`RegisterUpstream RemoveUpstream RegisterRepository ArchiveRepository RemoveRepository
RecordEvents BindPublication RegisterIdentity ReplaceKey RegisterKey SetKeyScope RevokeKey AddCredential SecretGrant Capability GrantClaim PauseSchedule
BindWorkflow Finish CreateIssue UpdateIssue ChangeIssueLock CloseIssue ReopenIssue DeleteIssue AcceptBatch
ChangeGrant InstallAdmin PreparePublication StartPublicationAttempt SettlePublication AbandonPublication RetryPublication SaveProgress AdvanceNative PreparePromotion FinishPromotion Invite Enroll
AddCertificatePin ReserveInvitation Assist RecordAction UnarchiveRepository FailUnbound PutNodes CompleteEvidence ProjectCI ImportIssues
BeginEvidence Cancel RotateCredential BeginPurge FinishPurge RetainEvents pruneEvents`)
	silent := strings.Fields(`RetainSchedule PublishSchedule ForgetSchedule PruneSchedules`)
	known := make(map[string]bool)
	for _, name := range append(public, silent...) {
		known[name] = true
	}
	files, err := os.ReadDir(".")
	requireOK(t, err)
	for _, file := range files {
		name := file.Name()
		if !inventorySource(name) {
			continue
		}
		source, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		requireOK(t, err)
		for _, declaration := range source.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok || fn.Recv == nil {
				continue
			}
			if transactionEntry(fn) && !known[fn.Name.Name] {
				t.Errorf("classify notification and dependent readers for %s in %s", fn.Name.Name, name)
			}
		}
	}
}

func inventorySource(name string) bool {
	if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
		return false
	}
	for _, private := range []string{"token", "key", "credentials", "cosign"} {
		if strings.Contains(name, private) {
			return false
		}
	}
	return true
}

func transactionEntry(fn *ast.FuncDecl) bool {
	entry := false
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		method, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		receiver, ok := method.X.(*ast.Ident)
		if ok && receiver.Name == "s" && (method.Sel.Name == "observe" || method.Sel.Name == "change") {
			entry = true
		}
		return true
	})
	return entry
}
