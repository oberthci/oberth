package gitcache

import (
	"testing"
	"time"
)

func TestAdmissionTimeoutDefaultsToThirtySeconds(t *testing.T) {
	t.Parallel()
	c, _, _ := gateFixture(t)
	if c.config.AdmissionTimeout != 30*time.Second {
		t.Fatal("default admission timeout", c.config.AdmissionTimeout)
	}
}
func TestManagedReceiveCannotStartAutomaticMaintenance(t *testing.T) {
	t.Parallel()
	c, session, _ := gateFixture(t)
	spec := command{dir: session.store, args: []string{"config", "--get", "receive.autogc"}}
	value, err := c.capture(t.Context(), spec, 32)
	if err != nil || string(value) != "false" {
		t.Fatal("receive can maintain refs outside the gate", string(value), err)
	}
}
