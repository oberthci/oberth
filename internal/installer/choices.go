package installer

import (
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/workflowpolicy"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// Choices distinguishes unset values from explicit false and zero. Values uses
// JSON containers; preparation takes an owned snapshot of nested maps and lists.
type Choices struct {
	StoreAddress      string
	Context           string
	URL               string
	SSHPort           int
	HTTPSNodePort     int
	SSHNodePort       int
	Namespace         string
	PipelineNamespace string
	Parallel          *int
	PodFloor          *bool
	Egress            *bool
	Store             *bool
	// SourcePins adds consumer and producer pairs to the saved source pin
	// policy; ClearSourcePins drops the saved pairs first.
	SourcePins      workflowpolicy.SourcePolicy
	ClearSourcePins bool
	Values          map[string]any
}

const defaultNamespace = "oberth"

func Defaults() Choices {
	parallel, floor, egress, store := 4, true, true, true
	return Choices{
		SSHPort:           30022,
		HTTPSNodePort:     30443,
		SSHNodePort:       30022,
		Namespace:         defaultNamespace,
		PipelineNamespace: pipelineNamespace(defaultNamespace),
		Parallel:          &parallel,
		PodFloor:          &floor,
		Egress:            &egress,
		Store:             &store,
	}
}

func resolve(c Choices, host string) (Choices, error) {
	d := Defaults()
	if c.Namespace == "" {
		c.Namespace = d.Namespace
	}
	if c.PipelineNamespace == "" {
		c.PipelineNamespace = pipelineNamespace(c.Namespace)
	}
	if c.HTTPSNodePort == 0 {
		c.HTTPSNodePort = d.HTTPSNodePort
	}
	if c.SSHNodePort == 0 {
		c.SSHNodePort = d.SSHNodePort
	}
	if c.SSHPort == 0 {
		c.SSHPort = c.SSHNodePort
	}
	if c.URL == "" {
		if loopbackHost(host) {
			return c, problem("loopback-url", nil)
		}
		c.URL = (&url.URL{Scheme: "https", Host: net.JoinHostPort(host, strconv.Itoa(c.HTTPSNodePort))}).String()
	}
	if err := checkNamespaceChoices(c); err != nil {
		return c, err
	}
	if err := checkChoices(c); err != nil {
		return c, err
	}
	c.URL = strings.TrimSuffix(c.URL, "/")
	return c, nil
}
func validateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return problem("url", nil)
	}
	if strings.ContainsAny(raw, "?#") || (u.Path != "" && u.Path != "/") || !validPort(u.Port()) {
		return problem("url", nil)
	}
	if unspecifiedHost(u.Hostname()) {
		return problem("address", nil)
	}
	if apicontract.ASCIIHost(u.Hostname()) {
		return nil
	}
	ascii, err := apicontract.PunycodeAddress(u)
	if err != nil {
		return described("url", err, "Host has no valid punycode form.")
	}
	return described("url-host", nil, "--url "+ascii)
}

// Numeric zero spellings are classified locally; hostnames are never resolved.
func unspecifiedHost(host string) bool {
	host = strings.TrimSuffix(host, ".")
	if address, err := netip.ParseAddr(host); err == nil {
		return address.WithZone("").Unmap().IsUnspecified()
	}
	parts := strings.Split(strings.ToLower(host), ".")
	if len(parts) > 4 {
		return false
	}
	for _, part := range parts {
		part = strings.TrimPrefix(part, "0x")
		if part == "" || strings.Trim(part, "0") != "" {
			return false
		}
	}
	return true
}
func validPort(port string) bool {
	if port == "" {
		return true
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}

func checkChoices(c Choices) error {
	if c.Context == "" {
		return problem("context", nil)
	}
	if err := validateURL(c.URL); err != nil {
		return err
	}
	code := ""
	switch {
	case c.SSHPort < 1 || c.SSHPort > 65535:
		code = "ssh-port"
	case !validNodePorts(c):
		code = "node-port"
	case c.Parallel != nil && *c.Parallel < 0:
		code = "parallel"
	}
	if code != "" {
		return problem(code, nil)
	}
	return nil
}
func deployedChoices(c Choices, stored map[string]any) Choices {
	server, _ := stored["server"].(map[string]any)
	if c.PipelineNamespace == "" {
		pipeline, _ := stored["pipeline"].(map[string]any)
		c.PipelineNamespace, _ = pipeline["namespace"].(string)
	}
	ports, _ := stored["nodePorts"].(map[string]any)
	if c.HTTPSNodePort == 0 {
		c.HTTPSNodePort = number(ports["https"])
	}
	if c.SSHNodePort == 0 {
		c.SSHNodePort = number(ports["ssh"])
	}
	public, _ := server["public"].(map[string]any)
	if c.URL == "" {
		c.URL, _ = public["url"].(string)
	}
	if c.SSHPort == 0 {
		c.SSHPort = number(public["ssh_port"])
	}

	return c
}

func number(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

func validNodePorts(c Choices) bool {
	return c.HTTPSNodePort >= 1 && c.HTTPSNodePort <= 65535 && c.SSHNodePort >= 1 && c.SSHNodePort <= 65535 && c.HTTPSNodePort != c.SSHNodePort
}

func validNamespaces(c Choices) bool {
	return len(validation.IsDNS1123Label(c.Namespace)) == 0 && len(validation.IsDNS1123Label(c.PipelineNamespace)) == 0
}

func reservedNamespace(name string) bool {
	return name == "default" || strings.HasPrefix(name, "kube-")
}

func checkNamespaceChoices(c Choices) error {
	switch {
	case reservedNamespace(c.Namespace) || reservedNamespace(c.PipelineNamespace):
		return problem("namespace-reserved", nil)
	case !validNamespaces(c):
		return problem("namespace", nil)
	case c.Namespace == c.PipelineNamespace:
		return problem("namespaces", nil)
	}
	return nil
}

func loopbackHost(host string) bool {
	address, err := netip.ParseAddr(host)
	return err == nil && address.WithZone("").Unmap().IsLoopback() || strings.EqualFold(strings.TrimSuffix(host, "."), "localhost")
}
