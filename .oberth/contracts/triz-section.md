## TRIZ contract

- **Main useful function (MUF)**: the one thing the system is for. A build service preserves the integrity of source, checks and released artifacts.
- **Harmful function**: the system does something it should not (unchecked
  source mutation, expired build receipt, corrupt artifact bytes). **Insufficient**: it does the right
  thing weakly (source validation only in one build lane). **Excessive**: it does more than
  the function needs (repeated caveats, redundant compatibility code).
- **Contradiction**: improving A worsens B (technical), or X must be A and
  not-A (physical). Naming the contradiction is the first step of any change
  that adds a mode, flag, alias, profile or escape valve.
- **Separation**: how a physical contradiction is resolved without compromise:
  in space (different places), in time (different moments), by condition
  (different inputs), or by system level (move it up or down). A compromise
  ("enforce by default, but allow off") is not a resolution; it is the
  contradiction left open with a flag on it.
- **Ideality**: useful functions divided by harmful functions plus cost. A
  change that adds a part must say which function it adds or which harm it
  removes.
- **Resource**: something already in the system that can carry a function for
  free (the build receipt already carries the source digest and check results).
- **Trimming**: remove a part and name what carries its function afterwards.
- **Guard**: the test, lint, generator or schema that makes a rule mechanical.


The contract guard runs in `.oberth/build.yaml` as `contract-guard`.
Canonical owner: Oberth.
The emitted guard is a build artifact. Regenerate it from the owner when
changing rules; review repository baselines separately.

| Law or rule | Guard and registry |
| --- | --- |
| 1. One owner per fact | `.oberth/contracts/check.py`: confession wording, generator exception |
| 2. Claimed equals measured | registered build outputs; `.oberth/contracts/generated-files.json` runs registered generators in check mode |
| 3. A mode needs a contradiction and separation | env, annotation and flag registry: `surface-registry.json`; chart settings are registered as values |
| 4. Every deprecation has a sunset | `deprecations.yaml`: required fields and date/version expiry; unregistered compatibility wording ratchet |
| 5. Scratch is not record | path and content residue ratchet across tracked source and documentation |
| 6. Composition roots | 200-line function span ratchet; parser/AST imports in Go tests prohibited |
| 7. Memory hygiene | unsafe and zeroing patterns outside memory hygiene packages ratcheted |
| 8. Test seams | test/skip/insecure/development env reads ratcheted |
| 9. Lifecycle on the default profile | review requires the qualified test cell; no live qualification is implied by this lint |
| 10. Gates have setters | literal Go test gates require a pipeline setter and a lane in skip diagnostics |
| 11. Specific, explained suppression | suppression and broad security-exclusion ratchets |

Existing findings are recorded as exact content fingerprints and per-rule
counts in `.oberth/contracts/baseline.json`. A removed finding cannot be
replaced by a different violation. New surfaces require a documentation anchor
and a qualified test cell. Empty registries record no approved exceptions;
existing debt remains visible in the baseline, not silently grandfathered by
file or directory. Reduce the baseline when removing debt.

The review checklist is `.gitea/pull_request_template.md`; issues use
`.gitea/ISSUE_TEMPLATE/contract.md`. Incorrect answers about copied facts,
unmeasured claims or missing sunsets block promotion.
