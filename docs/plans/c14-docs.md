# C14 — Documentation

## Purpose

C14 explains the current product and provides a checked path through install, connect,
source-consuming CI, logs, UI, promotion and release. It owns current prose, generated
references and the separation of current instructions from release history. Component
owners define the behavior it describes.

## Owned data and public contract

- **Delta:** The root README is one default-path entry point, linking detailed guides
  rather than repeating policy. Explain eligible CI issue creation and the difference
  between ordinary tags and releases.
- Source guides describe admitted native input artifacts, declared tools, volumes and
  cache settings; no unfinished cutover narrative.
- Ordinary tags use build CI; version tags and explicit routes use the release document.
  Ambiguity refuses before execution.
- Main-source examples show the portable `safe.directory=/work/src` setting and explain
  C5's narrow automatic hook. Effective working directories under `/work/src` require
  the declared source, unless covered by an authored volume. Pins contain no `.git`
  directory.
- Grants name the exact repository, workflow file and step. Show the authored token
  projection, audience, public trust and client. Declarations and annotations grant no
  authority.
- Describe the immutable admitted plan, pending entries, retries folded into one logical
  step and dynamic lower-bound counts. Public fields include `step_count` and
  `steps_planned_exact`.
- Events documentation follows C2's envelope, cursor gaps, reconnect and
  observation-only `run.updated`, with its separate retention. Publication and run
  success remain distinct facts.
- Follow [C9's command inventory](c9-install-and-upgrade.md#owned-data-and-public-contract)
  and [backup contract](c9-install-and-upgrade.md#backup) for maintenance guidance. Cutover is an
  operator procedure outside the product. Run history starts fresh.

## Allowed dependencies

C14 has no runtime imports. Documentation consumes public CLI help, C1/C2 schemas and
the owning component contracts. Examples use C4's production admission path. **Delta:**
C13 CI runs generated help/registry checks, whole-current-corpus symbol/link checks and
executable examples. Their exact permissions live in
[dependencies.json](dependencies.json).

## Invariants and budgets

1. Commands, flags, fields, tools, events and local links resolve against the current
   build. Generate CLI, tool and message references from their owner catalogues;
   `TestGeneratedReferencesCurrent` refuses stale checked-in references.
2. Runnable examples are complete checked files. Label excerpts and link their complete
   fixtures; unknown live facts never become a pass.
3. Ref and tag values enter shell steps as quoted arguments, never script text.
4. Messages follow the 40/80/120 contract with full native detail separate.
5. **Delta:** Document the policy defaults and limits defined by
   [C9 invariant 7](c9-install-and-upgrade.md#invariants), including review, opt-out
   and the need for a network plugin that enforces NetworkPolicy.
6. Argo owns execution, scheduling, overlap, dependencies and authored retries. The only
   run cap is its global cap; the server adds no capacity queue.
7. Quote owner budgets: 512 KiB submitted-source spec, 2048 entries / 1 MiB plan with
   dynamic placeholders, and at least 1856 MiB server source scratch.
8. Separate measured cold/warm results from configured resource budgets. Old release
   evidence does not qualify the current release. C14 adds no runtime budget or second
   validator.

## Inspections

The requirement sentence is the inspection contract. These inspections are planned.

| Requirement | Inspection |
| --- | --- |
| [REQ-C14-001](REQUIREMENTS.md#req-c14-001) | `TestCurrentPublicDocumentationContract` (contract_test) |

`TestGeneratedReferencesCurrent` checks the three generated references.
`TestCurrentPublicDocumentationContract` enumerates current pages and runnable examples,
checks commands/help/tool schemas and local links, and passes ref/tag values as quoted
arguments rather than script text. Examples use the same admission and read-only run
check as submission. The install/connect/push/log/UI/promote/release walks execute the
documented commands on the current release with real source delivery.

## Retired terms

Old architecture narrative → release/archive history; duplicated command lists →
generated references; unchecked excerpts → complete executable fixtures.
