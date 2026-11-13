# Single-host TLS names

For a selected cluster with one node, the installer and plan use that node's
names and addresses: `localhost`, `127.0.0.1`, `::1`, the node hostname and its
short `.local` name, and its advertised LAN/global addresses. The installer
never substitutes the laptop's interfaces for a remote node's addresses.
Explicit `--tls-extra-dns-name` and `--tls-extra-ip` values are retained.
Unspecified, multicast and link-local addresses are excluded. More than one
node does not trigger this discovery.

The plan JSON's Review fields show the resulting names. The wizard uses the
same discovery function in its cluster probe and shows them at Review.
Host installation prints the resolved names before Helm or identity changes.
Existing CA permissions are never silently widened. If replacement is needed,
review the full name set and pass `--reissue-server-ca` with the install or
upgrade. The CA pin changes and clients must refresh their trust. An existing
CA with IP constraints is never replaced by the DNS-only generator.

## DNS and IP client connections

The owner decision for #1559 allows clients to connect by any IP carried as
an IP SAN on the server certificate. Choose a detected node address or enter
a custom node/load-balancer IP under Extra names in setup, then select it as
the client address. With the CLI, set `--client-host` to the chosen IP. The
installer includes that choice in the leaf IP SANs and displays its coverage
in plan, Review and the install summary. IPv6 literals are bracketed in URLs,
for example `https://[2001:db8::10]:30443/mcp`.

DNS names, including `localhost` and names resolved through DNS, hosts files
or mDNS, remain supported. DNS choices need matching DNS identity; IP choices
need a matching IP SAN. A CA pin establishes the authority, not the endpoint's
identity. Neither path bypasses certificate verification.

Reachability by a node's IP depends on the network between client and node.
Guests behind NAT isolation cannot reach each other. Use port-forward, or a
name or IP routable from each client. A DNS alias does not fix missing routing.

This change leaves the production OpenBao CA generator and existing CA name
constraints unchanged. Its DNS constraints restrict DNS identities; they are
not CA-level IP constraints. Leaf IP SANs are distinct from CA-level IP name
constraints. No CA replacement or new CA-level IP constraint support is
implied by selecting an IP endpoint. Existing IP-constrained CAs retain their
constraints. Clients must still verify the leaf's exact IP SAN and the CA.
