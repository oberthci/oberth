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

## DNS-only client connections

The owner decision for #1530 (2026-11-11) defers CA-level IP name constraints
until Bun releases a fix. Keep the production CA's DNS constraints. Clients
connect by DNS name: `localhost` on the node, the host name, or `host.local`
when that name resolves from the client. Set `--client-host` to that DNS name;
do not use `127.0.0.1`, `::1`, or a LAN IP in an HTTPS/MCP client URL. The
DNS name must be covered by the server certificate and the CA constraints.
IP discovery and IP SANs do not authorize using an IP as a client endpoint.
TLS verification and the explicit CA pin remain required.

The pinned OpenBao 2.6.1 generation API exposes
[`permitted_dns_domains` only](https://raw.githubusercontent.com/openbao/openbao/v2.6.1/builtin/logical/pki/fields.go).
It cannot generate CA-level `/32` and `/128` IP constraints with its internal,
non-exportable key. Issuing-role restrictions on IP SANs are not CA-level
constraints. A future IP-connection change therefore needs both backend
support and a released [Bun fix](https://github.com/oven-sh/bun/pull/45236),
followed by verification with the actual client. These are deferred scope,
not release gates for the approved DNS-only connection path.

The existing self-signed server anchor's tests exercise critical DNS and
exact IPv4/IPv6 constraints. They do not establish production OpenBao IP
constraints or affected Bun client support and do not change the DNS-only
client connection policy.
