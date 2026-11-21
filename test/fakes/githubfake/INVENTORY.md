# GitHub-kind upstream inventory

Reference: `ref-old` commit `be320e877237b57fbb3eec5d899c850101097d75`.
All paths below are relative to that read-only checkout. Inventory precedes
implementation. Scope includes the server, repository helper and authored
workflows. Searches included hidden workflow files, Go HTTP clients, command
execution, upstream-kind branches and every occurrence of upstream API hosts,
GraphQL, webhooks, issue sync and release publication. No live upstream was used.

## Git (same protocol for GitHub-kind and Forgejo-kind)

| Interaction and evidence | Request sent | Response consumed / distinguished errors |
| --- | --- | --- |
| Bootstrap host-key scan: `internal/app/upstream_bootstrap.go:953`, `:995` | SSH handshake, deliberately without authentication; offer each supported host-key family | Presented public host key; handshake may fail authentication after key capture. Published fingerprints at `:498` differ by public forge host. Unknown local endpoints require an explicit expected fingerprint (`:547`); never impersonate a public hostname. |
| Authentication/readiness: `internal/app/upstream_bootstrap.go:1029`, `:1042`; `internal/app/upstream_status.go:72`, `:139-154`, `:188` | SSH public-key authentication with configured user (default `git`), no channel | Successful authenticated connection only; no greeting parsed. Wrong key, changed/unpinned host key, network error, cancellation and deadline are distinct. Shared or dedicated deploy identity is selected locally; no API lookup. |
| Repository verification: `internal/app/upstream_repo_probe.go:64`, `:99` | SSH exec `git-upload-pack '/org/repo.git'`; read first pkt-line; send `0000` and close stdin | Valid pkt-line proves readable repository, including empty repository. `ERR ` pkt-line, EOF, stderr/exit failure become repository-not-found; transport/authentication failure occurs before service start. |
| Initial population/refresh: `internal/gitcache/cache.go:353`, `:501`, `:823` | Local bare init, then remote `fetch --prune --no-tags upstream +refs/heads/*:<private-prefix>heads/* +refs/tags/*:<private-prefix>tags/*`; `ls-remote --symref upstream HEAD` | Real Git objects and ref advertisements; symref/default-branch name and object IDs. Empty remote is valid. Missing ref/repository differs from failed transport; failed refresh cannot authorize stale contents. Initial clone is implemented as init + fetch, not remote `clone`. |
| Promotion reads: `internal/gitcache/upstream.go:203`, `:276`; `promotion_train.go:79`; `promotion_tag.go:158` | Fetch exact branch, `ls-remote --heads upstream refs/heads/<branch>`; `fetch --no-tags upstream refs/heads/<branch>` | Exact target object, empty successful advertisement means absent branch; network failure is never absence. Local workspace clone/fetch (`workspace.go:25`) does not contact an upstream. |
| Branch publication: `internal/gitcache/upstream.go:53`, `:320`, `:330`; `publication.go:32` | `push [--force] upstream <sha>:refs/heads/<branch>`; promotion is nonforced; read back with `ls-remote --heads` or `--refs` | Receive status plus exact read-back SHA. Non-fast-forward, concurrent movement, permission/authentication refusal are permanent; transport/outage errors can retry. |
| Tag publication: `internal/gitcache/upstream.go:89`; `promotion_tag.go:72` | Nonforced `push upstream <object>:refs/tags/<tag>`, then `ls-remote --tags`/`--refs` | Exact tag **object** ID, preserving annotated tags and peeled commit distinction. Existing/moved tag refusal differs from transient outage. |
| Ref deletion: `internal/gitcache/branch_deletion.go:568`, `:683` | `ls-remote --heads`; chunked `push --force-with-lease=refs/heads/<branch>:<old> upstream :refs/heads/<branch> ...`; read back | Per-ref disappearance, already absent, stale lease/concurrent advance, refusal, or unavailable. Archived refusal recognized at `:439`: GitHub-kind text says repository was archived so it is read-only; Forgejo-kind text says repo is archived. |
| Release auxiliary repository: `.oberth/release.sh:596`, `:603`, `:663`, `:668` | Host-key scan on SSH port 443, `clone --depth 1`, ordinary `push origin HEAD:main`, fresh shallow clone for read-back | Hardcoded published host fingerprint and file bytes; fail on wrong key, failed clone/push or mismatched file. Walk must replace authored endpoint/pin with local values. No release REST API. |
| Workflow baseline fetch: `.oberth/build.yaml:182` | `fetch --no-tags --depth=1 https://<upstream>/oberthci/oberth.git main` after bare init | Git smart HTTP advertisement and upload-pack; exact objects for local inspection. This authored workflow uses a general Git executable. |

Server transport uses SSH exec `git-upload-pack '<path>'` and
`git-receive-pack '<path>'`, optionally SSH env `GIT_PROTOCOL=version=1|version=2`
(`internal/gitssh/ssh.go:127-131`). Paths may start `/` or `~/`
(`internal/app/upstream_repo_probe.go:22`). Server upstream validation explicitly
rejects HTTPS (`internal/gitcache/validation.go:193`); both upstream kinds share
that rule. The fake supplies SSH for the server and smart HTTP for the authored
workflow baseline fetch. Both delegate wire protocol, object storage, lease
checks, shallow reads, tagging and deletion to real Git, never simulated JSON.

`internal/gitssh/failure.go:15` distinguishes policy refusal, temporary transport
failure and remote service failure; cancellation remains cancellation.
`internal/pushretry/retry.go:15` limits attempts to five, with 2/4/8/16 minute
backoffs. `:48` refuses retries for mixed permanent/transient errors, cancellation,
non-fast-forward, protected refs, permission/authentication/access refusal and
repository-not-found. Deadlines, temporary SSH stream errors, disconnect/reset,
DNS/dial errors and 500/502/503/504 text are transient (`:61-70`). The fake offers service
outage, forbidden, not-found, archived, protected, slow and dropped-connection
faults; real Git supplies non-fast-forward and stale-lease errors.

Existing oracle tests reproduced by shape, never copied into a temporary module:
`internal/app/upstream_bootstrap_test.go:241` (handshake without channels), `:297`
(auth refusal); `internal/app/upstream_repo_probe_test.go:108` (missing-path advertisement probe);
`internal/gitcache/promotion_tag_test.go:12` (annotated tag publication);
`internal/gitcache/branch_deletion_test.go:183`, `:242` (compare and race);
`internal/pushretry/retry_test.go:13`, `:57` (classification).

## HTTP surfaces

| Endpoint and evidence | Request / response / errors | Shape source |
| --- | --- | --- |
| `POST /api/v3/repos/{owner}/{repo}/keys`: the repository helper `add-repo:202-204` | Helper runs authenticated CLI POST with JSON `{"title":"Oberth","key":"<canonical public line>","read_only":false}`. Output discarded; client reads success/failure exit only, no fields. Return 201 with deterministic `id`, `key`, `title`, `read_only`, `verified`, `created_at`, `url`. Invalid key or duplicate returns 422; unseeded repository 404; inject 403, 404, 5xx, delays, disconnects, primary/secondary throttling. The helper first runs CLI auth status; its precise transport probes are listed below, including the enterprise prefix. | Request/consumption from script. Success/error envelope from [deploy-key API documentation](https://docs.github.com/en/rest/deploy-keys/deploy-keys?apiVersion=2026-03-10). |
| `GET /{owner}/{repo}/releases/download/{tag}/{filename}`: `.oberth/build.yaml:724`, `:915`, `:3083`, `:3239`; `.oberth/release.yaml:767`; `hack/argo-live-cluster.sh:53`; `hack/scan-static-git.sh:24`; `examples/generic/.oberth/build.yaml:123`; `examples/go-project/.oberth/release.yaml:166` | Workflow/tool bootstrap downloads pinned bytes and checks digests or parses an installation document. Return seeded bytes directly, no external redirects. Unknown asset 404. Faults cover HTTP errors, slow reads and disconnect. Arbitrary valid fixture bytes do not satisfy real tool pins: offline walk inputs must contain the actual approved dependency bytes. | Request paths and byte consumption from authored files; no REST JSON. Dependency identities are intentionally not repeated here. |
| Git smart HTTP: `.oberth/build.yaml:182` | `GET /{owner}/{repo}.git/info/refs?service=git-upload-pack`; `POST /{owner}/{repo}.git/git-upload-pack`, Git content types and optional `Git-Protocol`. No HTTP receive-pack client is present, so it is refused/counts as unexpected. | Real Git `http-backend`, determined by old command. |

Primary rate limit: HTTP 403 or 429, `X-RateLimit-Remaining: 0`, reset epoch,
JSON message. Secondary: HTTP 403 or 429, `Retry-After` and secondary-limit
message. Source: [public rate-limit documentation](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api).
The old helper fixtures `internal/installer/plugin_add_repo_test.go:157`,
`:168` and `:445` model CLI authentication as an exit-code precondition, pin
the POST arguments, and strip public-key comments before upload. The new
contract test reproduces that POST shape. No Oberth HTTP retry/rate-limit parser
exists in this checkout: the helper
handles all CLI failures by returning its manual-registration path and exit 2.
Rate-limit faults implement the brief's inspection contract; they do not imply
an existing issue-sync client. The fake allows both documented status choices.

## Explicit absences and kind differences

- Issues are Oberth-owned state: `internal/service/issue_lifecycle.go:16`,
  `internal/service/issue_workspace.go:1`, `internal/store/issues.go:1`.
  No outbound issue sync REST/GraphQL calls, pagination or upstream issue IDs.
- Repository metadata comes from Git advertisements/catalog, not REST.
  No `/repos/{owner}/{repo}` GET client, statuses/checks API, OAuth/App token API
  or server query request is present. The helper authentication precondition is
  described separately below.
- API route registration `internal/api/server.go:279-335` has no upstream
  webhook/callback receiver. Runs enter through Oberth's Git receive hooks
  (`internal/gitcache/native_hooks.go:1`), not upstream push events.
- Release publication uses repository Git plus separately configured release
  storage (`.oberth/release.sh:663`, `:958`, `:1491`); there is no GitHub-kind
  release create/edit/upload endpoint. Separate storage is outside this fake.
- Server upstream kind is transport-oriented, not a GitHub REST adapter.
  SSH routing/ref semantics are identical. Kind/host differences are published
  fingerprints (`internal/app/upstream_bootstrap.go:498`), setup links and
  default upstream URLs (`internal/setuptui/questions.go:437`, `:451`;
  `internal/installer/installer.go:699`), archived-error spelling above, and
  helper deploy-key dispatch through the repository enrollment flow.
  Forgejo-kind helper probes `/user` then POSTs `/repos/.../keys` through its
  own CLI; the GitHub-kind helper uses CLI auth status then the POST above.
  The local forge owns its API; the fake serves the scope probe and deploy-key POST.

## Transport details and helper probes

All API, Git HTTP and download requests are served over HTTPS with TLS >=1.2.
The CA is generated at startup or loaded, then written to a caller-selected
path. Only the control listener uses plain HTTP, strictly on loopback.

| Client evidence | Scheme / TLS floor | Redirects and retries |
| --- | --- | --- |
| `.oberth/build.yaml:706-724`, `:3079-3083`; `.oberth/release.yaml:748-767` | HTTPS, explicit `--proto =https --tlsv1.2` | `--location --proto-redir =https`; `--retry 2 --retry-all-errors`, connect 10 s / total 90 s per attempt. |
| `.oberth/build.yaml:915` | HTTPS with the preceding download template's HTTPS/TLS controls | Same authored retry/redirect policy as the first row. |
| `examples/generic/.oberth/build.yaml:110-123` | HTTPS, `--proto =https --tlsv1.2` | Follow redirects, two retries, 60 s attempt timeout. |
| `examples/go-project/.oberth/release.yaml:153-166` | HTTPS, `--proto =https --tlsv1.2` | Follow redirects, two retries, 90 s attempt timeout. |
| `hack/argo-live-cluster.sh:51-54` | HTTPS, `--proto =https --tlsv1.2` | Follow redirects, no explicit retry, connect 10 s / total 120 s, 16 MiB maximum file size. |
| `hack/scan-static-git.sh:24-28` | HTTPS URL; curl restricts `--proto =https` but has no explicit TLS-floor flag | Follow redirects, no explicit retry, 120 s timeout. Its alternative downloader uses the HTTPS URL and timeout with its native TLS/redirect defaults. |
| `.oberth/build.yaml:3239-3246` | HTTPS URL; inline `curl -fsSL` supplies no explicit TLS/protocol restriction | Follow redirects, no explicit retry, connect 10 s / total 40 s. |
| `.oberth/build.yaml:182` | Git smart HTTPS, native Git/libcurl TLS validation | Native Git redirect defaults; no authored retry. |
| Repository enrollment deploy-key registration | CLI HTTPS with its native TLS policy | CLI HTTP redirect policy; no helper-authored retry. Non-success exits into manual registration. |

Tests drive each curl profile against the local TLS endpoint with the emitted
CA. The strict profile also verifies HTTPS redirect following, refusal to
redirect to HTTP, recovery after one fault and visible failure after three
consecutive faults. Call history counts every request; faults expose attempts
and remaining counts. The fake itself never redirects externally.

The old server and the rebuild add no GitHub-kind web API client beyond this
inventory. Helper auth is the only extra transport surface, derived from its
actual invocation and the [public auth-status manual](https://cli.github.com/manual/gh_auth_status)
and [API manual](https://cli.github.com/manual/gh_api). For an explicit, reproducible
wire reference, the published CLI v2.83.1 source defines:

- `GET /api/v3/` with Authorization token: require 200 and read X-Oauth-Scopes;
  the body is discarded. See [scope-probe implementation](https://raw.githubusercontent.com/cli/cli/v2.83.1/pkg/cmd/auth/shared/oauth_scopes.go)
  and [enterprise URL construction](https://raw.githubusercontent.com/cli/cli/v2.83.1/internal/ghinstance/host.go).
  Fake response: empty JSON object and `repo, read:org, gist` scope header.
- With an environment token, auth status first asks for the login through
  `POST /api/graphql` with JSON query `query UserCurrent{viewer{login}}` and
  no variables. It reads `data.viewer.login`. See [auth-status account flow](https://raw.githubusercontent.com/cli/cli/v2.83.1/pkg/cmd/auth/status/status.go)
  and [login query definition](https://raw.githubusercontent.com/cli/cli/v2.83.1/api/queries_user.go).
  This endpoint is outside the fake: the walk supplies the reference fixture
  authentication precondition. Every query endpoint is refused and counted.
- The helper then POSTs keys to `/api/v3/repos/{owner}/{repo}/keys` with the
  previously inventoried JSON. Un-prefixed API equivalents are refused and
  counted as unexpected.

This is a source-derived contract, not proof driven by the real CLI. The old
helper pins no CLI version, and no CLI binary was downloaded for validation.
Real CLI execution remains an explicit integration open point. Ordinary
GitHub-kind server API traffic is not being expanded to a general web client.

SSH clients can send LANG/LC_* (also COLORTERM/NO_COLOR); these are quietly
refused, not unexpected. The standard OpenSSH session-limit notification is
accepted. Public-key authentication is the only offered method. One auth-log
callback counts wrong-user failures and any attempted method other than
none/publickey. Unsupported-method-only clients fail at the expected no-auth
probe without increasing the count. Failed public-key checks for `git` are
expected, including injected refusals. Default host keys cover all three scanned families.
A repo that a deploy key cannot see returns repository-not-found; a read-only
key attempting to write its own repo receives a write refusal.

Hosted chart repositories (`internal/installer/installer.go:56`, `:66` and
`internal/installer/argo.go:27`) and registry images
(`internal/installer/rekorchart/values.yaml:128-131`, `charts/trillian/values.yaml`)
belong to the separate chart/registry stand-ins, not this upstream fake.
