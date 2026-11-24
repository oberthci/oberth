package main

import (
	"flag"
	"strconv"
	"strings"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/workflowpolicy"
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
		case "no-egress":
			value := !r.enabled(f.Name)
			c.Egress = &value
		case "external-store":
			value := false
			c.Store, c.StoreAddress = &value, f.Value.String()
		case "no-source-pins":
			c.ClearSourcePins = r.enabled(f.Name)
		case "source-pin":
			c.SourcePins = sourcePins(f.Value.(*sourcePinFlag).pairs)
		case "egress-allow", "egress-internet", "cluster-cidrs":
			if c.Values == nil {
				c.Values = map[string]any{"policies": map[string]any{}}
			}
			policy := c.Values["policies"].(map[string]any)
			switch f.Name {
			case "egress-allow":
				policy["allow"] = f.Value.(*egressDestinations).entries
			case "egress-internet":
				policy["internet"] = r.enabled(f.Name)
			case "cluster-cidrs":
				policy["clusterCIDRs"] = f.Value.(*clusterCIDRs).entries
			}
		}
	})
	return c
}

func sourcePins(pairs []string) workflowpolicy.SourcePolicy {
	policy := workflowpolicy.SourcePolicy{}
	for _, pair := range pairs {
		consumer, producer, _ := strings.Cut(pair, "=")
		policy[consumer] = append(policy[consumer], producer)
	}
	return policy
}
