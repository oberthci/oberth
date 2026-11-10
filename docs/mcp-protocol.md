# MCP version negotiation

Oberth's authenticated `POST /mcp` endpoint supports protocol versions
`2025-03-26` and `2025-06-18`. The preferred version is `2025-06-18`, also
used by the Oberth CLI. This is an explicit compatibility set; a newer dated
version is not automatically supported.

An `initialize` request must include a `params` object with a nonempty
`protocolVersion` string in `YYYY-MM-DD` form representing a real date. A
supported requested version is echoed. An unsupported dated version receives
the preferred supported version. The client must check that response before
continuing; if it cannot use that version, it should disconnect, as described
in the [MCP lifecycle specification](https://modelcontextprotocol.io/specification/2025-06-18/basic/lifecycle#version-negotiation).
Missing, null, non-string or malformed versions receive JSON-RPC error
`-32602` (invalid params), with the request's ID and no result. Client
capabilities, client information and extension metadata are accepted without
enabling any server-to-client features.

For example, these request params and result versions correspond:

| `params` | Result `protocolVersion` |
| --- | --- |
| `{"protocolVersion":"2025-03-26"}` | `2025-03-26` |
| `{"protocolVersion":"2025-06-18"}` | `2025-06-18` |
| `{"protocolVersion":"2099-01-01"}` | `2025-06-18` |
| `{}` or `{"protocolVersion":null}` | Error `-32602` |

Clients should initialize, inspect the response, then send
`{"jsonrpc":"2.0","method":"notifications/initialized"}` before using tools.
That notification receives HTTP 202 with an empty body. `initialize` needs
a request ID; sending it as a notification receives HTTP 400.
`notifications/initialized` must be a notification; sending it with an ID
receives JSON-RPC error `-32600` and performs no tool operation.

`POST /mcp` stays stateless for tools: a tool call needs no session, and
initialization stores no version or permission state by bearer token, network
connection or client address. Repeated initialization is allowed and does not
change another request's version or permissions. Existing authenticated
direct calls, including calls before initialization or after a failed
initialization, remain supported. Initialization is a compatibility exchange,
not an authorization gate; every request and notification still passes the
normal authentication gate, and tool calls retain their scope checks and drain
refusal.

When agent events are available (the normal server), the `initialize`
response also carries an `Mcp-Session-Id` header. The session exists only to
bind server push to the credential that opened it: an `events_subscribe`
filter, `run://<id>` resource subscriptions and the `GET /mcp` event stream.
A request that sends no `Mcp-Session-Id` is served without a session. A
session ID owned by another credential, or a malformed or repeated header,
answers HTTP 404. An unknown well-formed ID (for example one issued before a
restart) is adopted for the same credential by `resources/subscribe` and
`events_subscribe`, and otherwise ignored. See
[agent events](agent-events.md#push-to-a-connected-session).

On subsequent requests, clients using `2025-06-18` must send
`MCP-Protocol-Version` with the selected version. Either supported header
value is accepted; an unsupported, malformed, empty or repeated header receives
HTTP 400 before a tool can run, including on initialization requests. When the
header is absent, the endpoint retains
`2025-03-26` compatibility for existing direct callers. The initialization
body selects the version during negotiation; a supported initial version
header does not override it. The header validation follows the
[HTTP version-header specification](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports#protocol-version-header).

`initialize` advertises these capabilities:

- `tools`, always;
- `experimental["claude/channel"]` and `resources: {subscribe: true,
  listChanged: false}` when agent events are available. Events are pushed as
  `notifications/claude/channel` and `notifications/resources/updated` on the
  session's `GET /mcp` stream; `resources/list`, `resources/templates/list`,
  `resources/read`, `resources/subscribe` and `resources/unsubscribe` serve
  `run://<id>` resources. Without agent events these methods return `-32601`
  (method not found).

Prompts are not advertised, and their methods return `-32601`. Declaring
`claude/channel` does not make a client display the pushed events: Claude
Code runs a channel from a server it was not shipped with only when the
session is started with `--dangerously-load-development-channels
server:<name>` (see [agent events](agent-events.md#claude-code)).
