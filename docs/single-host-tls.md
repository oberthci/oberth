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

## Blocked production IP-constraint acceptance

The complete #1530 requirement is **blocked**, not satisfied by issuing-role
restrictions. The pinned OpenBao 2.6.1 generation API exposes
[`permitted_dns_domains` only](https://raw.githubusercontent.com/openbao/openbao/v2.6.1/builtin/logical/pki/fields.go).
It cannot generate the requested CA-level `/32` and `/128` IP constraints with
its internal, non-exportable key. Oberth's issuing role limits issued IP SANs,
but that is not equivalent to a constraint in the trusted CA certificate.
This branch retains the production CA implementation and does not send an
unsupported parameter that OpenBao would ignore. Backend support is required
before that acceptance criterion can pass.

There is a separate client blocker as of 2026-11-11: affected Bun runtimes,
including the runtime reported in Claude Code, reject valid chains with IP
name constraints. The upstream [Bun fix remains open](https://github.com/oven-sh/bun/pull/45236).
Do not disable TLS verification or remove constraints to claim this works.

The existing self-signed server anchor does carry critical DNS and exact IPv4
and IPv6 constraints. Tests verify allowed addresses and reject a child
certificate for another DNS name or IP. These tests do not establish that the
production OpenBao CA carries IP constraints or that affected Claude runtimes
can trust it.
