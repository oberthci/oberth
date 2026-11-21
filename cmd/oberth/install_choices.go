package main

import (
	"flag"
	"strconv"

	"github.com/oberthci/oberth/internal/installer"
)

func (r installationRequest) text(name string) string {
	if f := r.flags.Lookup(name); f != nil {
		return f.Value.String()
	}
	return ""
}
func (r installationRequest) enabled(name string) bool {
	value, _ := strconv.ParseBool(r.text(name))
	return value
}
func (r installationRequest) choices() installer.Choices {
	c := installer.Choices{Context: r.text("context"), URL: r.text("url")}
	c.Namespace, c.PipelineNamespace = r.text("namespace"), r.text("pipeline-namespace")
	c.HTTPSNodePort, _ = strconv.Atoi(r.text("https-node-port"))
	c.SSHNodePort, _ = strconv.Atoi(r.text("ssh-node-port"))
	c.SSHPort, _ = strconv.Atoi(r.text("ssh-port"))
	r.flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "parallel":
			value, _ := strconv.Atoi(f.Value.String())
			c.Parallel = &value
		case "no-pod-defaults":
			value := !r.enabled(f.Name)
			c.PodFloor = &value
		case "no-link-local-filter":
			value := !r.enabled(f.Name)
			c.Egress = &value
		case "external-store":
			value := false
			c.Store, c.StoreAddress = &value, f.Value.String()
		}
	})
	return c
}
