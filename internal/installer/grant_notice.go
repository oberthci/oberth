package installer

import (
	"fmt"
	"io"
)

func printGrantMirrorUpgradeNotice(w io.Writer, selected, namespace string) {
	if w == nil {
		return
	}
	if selected == "" {
		selected = "<context>"
	}
	if namespace == "" {
		namespace = DefaultNamespace
	}
	_, _ = fmt.Fprintln(w, "Secret grants now live only in the database. The old ConfigMap oberth-secret-access is left orphaned and ignored.")
	_, _ = fmt.Fprintf(w, "You may delete it: kubectl --context %s -n %s delete configmap oberth-secret-access\n", shellQuoteWord(selected), shellQuoteWord(namespace))
}
