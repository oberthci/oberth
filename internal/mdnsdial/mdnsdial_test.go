package mdnsdial

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"
)

func TestIsLocalName(t *testing.T) {
	for host, want := range map[string]bool{
		"tuxbox.local": true, "TuxBox.Local.": true, "a.b.local": true,
		"local": false, ".local": false, "tuxbox": false, "tuxbox.localdomain": false, "192.168.1.2": false,
	} {
		if got := IsLocalName(host); got != want {
			t.Errorf("IsLocalName(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestParseAhostsDeduplicatesSocketTypes(t *testing.T) {
	out := []byte("192.168.86.193  STREAM tuxbox.local\n192.168.86.193  DGRAM  \n192.168.86.193  RAW    \n10.42.0.1       STREAM \nfe80::1         STREAM \n\n")
	got := parseAhosts(out)
	want := []string{"192.168.86.193", "10.42.0.1", "fe80::1"}
	if len(got) != len(want) {
		t.Fatalf("parseAhosts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Fatalf("parseAhosts = %v, want %v", got, want)
		}
	}
}

func TestPreferLocalNetworksPutsLANBeforeBridge(t *testing.T) {
	_, lan, _ := net.ParseCIDR("192.168.86.0/24")
	ips := []net.IP{net.ParseIP("10.42.0.1"), net.ParseIP("192.168.86.193"), net.ParseIP("172.17.0.1")}
	got := preferLocalNetworks(ips, []*net.IPNet{lan})
	if got[0].String() != "192.168.86.193" || got[1].String() != "10.42.0.1" || got[2].String() != "172.17.0.1" {
		t.Fatalf("order = %v", got)
	}
}

func TestGetentLookupRefusesNonHostnames(t *testing.T) {
	for _, name := range []string{"-s.local", "a b.local", "a;b.local", "$(x).local"} {
		if _, err := getentLookup(context.Background(), name); err == nil {
			t.Errorf("getentLookup(%q) succeeded", name)
		}
	}
}

// offline is a dialer whose resolver never reaches a DNS server, so the
// tests do not depend on this machine's network.
func offline() Dialer {
	return Dialer{Base: &net.Dialer{Resolver: &net.Resolver{
		PreferGo: true,
		Dial: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("offline")
		},
	}}}
}

func TestDialerFallsBackForLocalNames(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(listener.Addr().String())

	var asked []string
	restore := Lookup
	t.Cleanup(func() { Lookup = restore })
	Lookup = func(_ context.Context, name string) ([]net.IP, error) {
		asked = append(asked, name)
		// The first address refuses; the dialer tries the next.
		return []net.IP{net.ParseIP("127.0.0.2"), net.ParseIP("127.0.0.1")}, nil
	}

	dialer := offline()
	var attempted []string
	// A second loopback address can blackhole for over a minute on macOS.
	// Inject the refusal before connect; the successful fallback still uses
	// the real listener, and both attempted addresses remain observable.
	dialer.Base.ControlContext = func(_ context.Context, _, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		attempted = append(attempted, host)
		if host == "127.0.0.2" {
			return syscall.ECONNREFUSED
		}
		return nil
	}
	conn, err := dialer.DialContext(context.Background(), "tcp", net.JoinHostPort("oberth-mdnsdial-test.local", port))
	if err != nil {
		t.Fatalf("dial = %v", err)
	}
	_ = conn.Close()
	if strings.Join(attempted, ",") != "127.0.0.2,127.0.0.1" {
		t.Fatalf("fallback attempts = %v", attempted)
	}
	if len(asked) != 1 || asked[0] != "oberth-mdnsdial-test.local" {
		t.Fatalf("Lookup asked %v", asked)
	}
}

func TestDialerKeepsDNSErrorWhenFallbackFails(t *testing.T) {
	restore := Lookup
	t.Cleanup(func() { Lookup = restore })
	Lookup = func(context.Context, string) ([]net.IP, error) { return nil, errors.New("no avahi") }

	_, err := offline().DialContext(context.Background(), "tcp", "oberth-mdnsdial-test.local:1")
	if !IsUnresolvedLocal(err) {
		t.Fatalf("err = %v, want an unresolved .local DNS error", err)
	}
}

func TestDialerDoesNotFallBackForOtherNames(t *testing.T) {
	restore := Lookup
	t.Cleanup(func() { Lookup = restore })
	Lookup = func(context.Context, string) ([]net.IP, error) {
		t.Fatal("Lookup called for a non-.local name")
		return nil, nil
	}
	_, err := offline().DialContext(context.Background(), "tcp", "oberth-mdnsdial-test.invalid:1")
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) || IsUnresolvedLocal(err) || !strings.Contains(dnsErr.Name, "invalid") {
		t.Fatalf("err = %v", err)
	}
}
