# Promotion trains

A train combines reviewed green branch commits and authorized parked commits into one promotion.
Oberth waits for earlier promotions to the same repository and target, fetches
the target's current head, and merges the sources in order in one temporary tree.
It runs **one promotion CI run** on the final commit. A green result publishes
that exact commit with the existing non-forced push checks. A red result leaves
the target unchanged.

Each source needs a full commit SHA whose newest run satisfies the same exactly
tested green branch check or authorized park provenance as single-source `promote`.
Parked sources require `repo` and follow the [park policy](park-promotion.md);
red branch evidence cannot be discarded by parking the same SHA. Optional `ref`
must match the resolved branch; it never selects a moving head instead of the requested SHA.
All sources belong to one repository. There must be 1–32 unique sources.
An admin uplink may promote directly; other uplinks need a valid promotion grant
for every source and the target, with repository opt-in also accepted for parked
sources. Only included sources' grants are consumed after
delivery. The existing `sha` form keeps its current merge and fast-forward behavior.
A `sources` request always runs CI, even with one source or an already-contained tree.

For a three-source train, put your actual tested commit SHAs in `sources.json`:

```json
[
  {"sha":"1111111111111111111111111111111111111111","ref":"feature/parser"},
  {"sha":"2222222222222222222222222222222222222222","ref":"feature/cache"},
  {"sha":"3333333333333333333333333333333333333333","ref":"feature/docs"}
]
```

Then submit it through the CLI:

```sh
oberth promotion create --repo codeberg/acme/widgets --branch main --sources-file sources.json
```

The MCP call uses the same ordered array:

```sh
oberth mcp call promote '{"repo":"codeberg/acme/widgets","branch":"main","sources":[{"sha":"1111111111111111111111111111111111111111","ref":"feature/parser"},{"sha":"2222222222222222222222222222222222222222","ref":"feature/cache"},{"sha":"3333333333333333333333333333333333333333","ref":"feature/docs"}]}'
```

REST accepts the same JSON object at authenticated `POST /api/promotions`, using
`Authorization: Bearer <uplink-token>`. All three forms return the durable
promotion `id`. Use `promote_status` to observe it or `promotion_list` to recover
an ID after a lost reply. Both expose `source_count`; it counts requested sources,
including skipped sources. Status includes `promotion.train.sources` in request
order and `promotion.train.tree_sha`, the Git tree object of the combined result.
`promotion.result_sha` remains the commit that CI tested and publication pushes.
The admission and plan audit entries retain the same source and tree evidence.

If a source conflicts, the failed promotion names its 1-based position, SHA and
conflicting paths. No CI run starts. Set `skip_conflicting: true` in the JSON
request, or pass CLI `--skip-conflicting`, to abort only the conflicting merge and
continue with the next source. Skips and paths stay in the existing promotion
reason line and durable train record, including after successful publication.
If every source conflicts, the promotion fails without a run. Git operational
errors, unavailable objects and authentication failures cannot be skipped.

`promote_retry` / `oberth promotion retry <id>` preserves the requested train and
conflict policy, rechecks every source and merges onto the current target again.
Train retries always test the resulting tree. A source that conflicted previously
is reconsidered. Queued requests use the existing promotion queue and statuses;
there is no separate train queue or run status.
