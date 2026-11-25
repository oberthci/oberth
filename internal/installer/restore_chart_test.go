package installer_test

import "testing"

func TestRestoreChartStopsServerAndUsesOneUseDatabasePod(t *testing.T) {
	t.Parallel()
	objects := rendered(t, map[string]any{"restorePending": true, "restoreMaxBytes": 2 << 30})
	deployment := object(t, objects, "Deployment", "oberth")["spec"].(map[string]any)
	if number := deployment["replicas"]; number != float64(0) {
		t.Fatal("pending restore started the server", number)
	}
	pod := object(t, objects, "Pod", "oberth-restore")["spec"].(map[string]any)
	if pod["restartPolicy"] != "Never" || pod["automountServiceAccountToken"] != false {
		t.Fatal("restore Pod can replay or has unnecessary cluster access")
	}
	container := pod["containers"].([]any)[0].(map[string]any)
	if container["stdin"] != true || container["stdinOnce"] != true || jsonOf(t, container["command"]) != `["/usr/local/bin/oberth","restore-database"]` {
		t.Fatal("restore Pod does not receive one checked database stream")
	}
	if jsonOf(t, container["args"]) != `["--destination","/var/lib/oberth/oberth.db","--schema-identity","oberth-schema-v1","--audit-domain","oberth-audit-v2","--max-database-bytes","2147483648"]` {
		t.Fatal("restore Pod lost database protocol or byte bound", container["args"])
	}
}
