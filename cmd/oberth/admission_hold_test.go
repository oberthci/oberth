package main

import (
	"errors"
	"strings"
	"testing"
)

func TestAdmissionHoldCLIForwardsDefaultsAndExplicitClasses(t *testing.T) {
	const token = "admission-test-token"
	server := newMCPEchoServer(t, token)
	defer server.Close()
	defer withTestMCPClient(t, server)()
	t.Setenv("OBERTH_TOKEN", token)
	t.Setenv("OBERTH_MCP_URL", server.URL)
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"hold", "on", "--reason", "release qualification"}, `tool=admission_hold args={"on":true,"reason":"release qualification"}`},
		{[]string{"hold", "--allow", "promotion,release", "on", "--reason", "release", "--duration", "12h", "--json"}, `tool=admission_hold args={"allow":["promotion","release"],"duration":"12h","on":true,"reason":"release"}`},
		{[]string{"hold", "on", "--reason", "drain", "--allow", ""}, `tool=admission_hold args={"allow":[],"on":true,"reason":"drain"}`},
		{[]string{"hold", "off", "--reason", "finished"}, `tool=admission_hold args={"on":false,"reason":"finished"}`},
	} {
		var output strings.Builder
		if err := runAdmission(t.Context(), test.args, &output); err != nil || !strings.Contains(output.String(), test.want) {
			t.Fatalf("%v = %s, %v", test.args, output.String(), err)
		}
	}
	for _, args := range [][]string{{"hold"}, {"hold", "on"}, {"hold", "maybe", "--reason", "x"}, {"hold", "on", "off", "--reason", "x"}} {
		if err := runAdmission(t.Context(), args, &strings.Builder{}); !errors.Is(err, errUsage) {
			t.Fatal(args, err)
		}
	}
}
