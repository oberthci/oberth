# Transport inspections

| Requirement | Inspection | Evidence |
| --- | --- | --- |
| REQ-C1-002 | TestPrivateBoundedDiagnosticsContract | Fixed diagnostic fields and credential-free failure replies. |
| REQ-C1-003 | ux-gate | `make check` runs all component catalogue tests and the whole-source literal/factory checks in `hack/guards`; HTTP/MCP tests preserve whole reason/remedy/detail. No name filter or runtime message walk. The browser walk remains separate. |
| REQ-C1-004 | ui | Browser implementation and pushed-run timing walk are outside this task. TestUIHandoffPreservesToolRunIdentityAndPlan verifies the transport half only. |

C2 implements the service and authentication ports; C11 wires them and runs their
contract suites. The UI consumes `RunSummary`, complete logical counts, native
attempt selectors, `LogResult.Meta`, public reason/remedy/detail fields and event
ID cursors. It must render whole reasons, show detail on demand, preserve token
isolation and check that a pushed run appears within five seconds. It supplies
the embedded shells/assets, escaping, content policy and browser behavior.

C11 sets ReadHeaderTimeout to 10 seconds and ReadTimeout to 30 seconds for
slow request bodies, and uses Server.WriteTimeout() for the write ceiling.
Config.FailureLog requires the injected private structured logger: unknown failures produce
one reference/cause entry; Diagnostics accepts only fixed transport facts. No
failure record is written to durable state. Native streams keep their own
five-second frame and thirty-second byte-write deadlines.

Operation deadline: C1 passes the declared remaining budget to C2; C2 stops and joins request work, retaining ownership of accepted durable actions.
Reply deadline: C1 installs the operation deadline plus five seconds for delivery, emits one timeout result and suppresses late success.
Connection deadline: C11 sets header/body timeouts and Server.WriteTimeout(); C1 resets native/frame writes to thirty/five seconds.
Shutdown deadline: C11 starts BeginDrain before its fifteen-second HTTP shutdown; C1 cancels all observer preparations, opens and active sources, and joins writers.

The service implements the HandlerCancellation contract with a real blocking
fixture. A noncooperative fixture may keep its request goroutine until explicitly
released; Go cannot terminate an arbitrary function. Its caller receives the
bounded timeout response before that release, and the worker is always joined.

The service consumes internal/apicontract for exact per-tool inputs/results,
typed ports and Required/Access authority. The table's ResultType is the same
O returned by Handler[I,O] and used for output schema construction. internal/api
exposes aliases; it owns no second DTO or class declaration. Native log read
values are in internal/logcontract and aliased by internal/runlog. C2 checks live
identity, hidden selectors, conditional policy and audit using that contract.
