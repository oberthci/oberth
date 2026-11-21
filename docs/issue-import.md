# Issue import

Ask the migration supplier for converted UTF-8 JSON Lines files and a separate `losses.jsonl`.
Oberth ships no issue-export command or exporter. The supplier converts the previous
installation's records to this format, lists every conversion/shortening in the loss file, and
retains that installation for full text. Never pass the loss file to `issue import`.

## Import order

Partition records into nonoverlapping descending number ranges: `issues-001.jsonl` has the highest
range, `issues-002.jsonl` the next lower, and so on. For example: 001 contains 301–450, 002
151–300, and 003 1–150. Source gaps are fine. Regroup overlapping chunks; renaming is not enough.
Use an empty installation before its first CI-enabled push. Import in ascending filename order; an
installation-wide administrator dry-runs and imports each file before starting the next:

```sh
oberth issue import issues-001.jsonl --dry-run
oberth issue import issues-001.jsonl
oberth issue import issues-002.jsonl --dry-run
oberth issue import issues-002.jsonl
```

A dry run validates records and existing numbers, writes nothing, and says "No issues imported."
Each file is atomic; open and closed issues keep their numbers. Importing the highest range first
advances native numbering beyond the whole export, avoiding later-file collisions even if native
issues are created between imports. This reserves nothing for arbitrary later exports. An existing
number refuses the whole file unless it is an exact complete semantic replay: "already imported"
preserves local edits. Record order and JSON whitespace do not matter; history array order does.
After a lost reply, repeat the same file to discover its result.

## Copyable record

[issues-001.jsonl](examples/issues-001.jsonl) includes a count header and one closed issue with
body, upstream, author, closer and ordered comment/state history. Copy it as a format example,
then replace all values with source records. Each record, including its history, occupies one
line. The authoritative format is [IssueImportRecord](../internal/model/issue_import.go),
alongside IssueImportHeader and IssueHistory; the server uses its ordinary strict JSON decoder.

The header declares the exact positive count. Each issue needs a unique integer `id` in
1–9223372036854775806, nonblank `title`, `state` (`open` or `closed`), and nonzero RFC3339
`created_at`. Use actual source numbers; the maximum leaves room for only one native issue.
Use `repo add --upstream <name>` before import, or map every source name with `--upstream <local>`;
keep that mapping on replay. Map empty or `global` explicitly. Closed issues require `closed_at`;
open issues omit it or use null. Optional `body`, `author` and `closer`
preserve source text, never credentials. Source attribution stays distinct after local edits.
Omitted `kind`, `work_status` and `occurrences` default to `manual`, `todo` and 1. Work status
accepts `todo`, `active`, `waiting`, `review`. `updated_at` defaults to the latest supplied time.
Supplied timestamps/offsets survive; history is never sorted by time. History entries have `kind`
(`comment`, `append`, `state`), nonzero `at`, author, optional `text`, and `state` (`open`,
`closed`) only for state entries. Missing attribution/text stays empty.

## Conversions, loss and limits

Only manual issues import. Convert CI issues to manual, starting the body with provenance naming
the original repository/ref. Record the kind change and original selectors once per issue in the
loss file. Later native failed CI creates a separate issue. Limits are **1048576 bytes per file
and encoded request**, **131072 per line**, and **131072 per complete projected issue**, including
history/provenance. Escaping can enlarge requests. Oberth never truncates. The supplier reserves
4096 bytes for lifecycle metadata and shortens to 126976 projected bytes: retain body first, then
newest history, dropping oldest entries first without reordering survivors. End the body with a
sentence saying it was shortened at export and full text remains in the previous installation
under the same number. [losses.jsonl](examples/losses.jsonl) shows the separate shortening record:
`id`, `original_bytes`, `kept_bytes`, `entries_dropped`, `original_body_bytes`, `kept_body_bytes`,
`full_text_location`. This is not import input. Record kind changes even without shortening.

Blank lines, null records, arrays, unknown fields, invalid UTF-8 and trailing JSON refuse. One
final newline is optional. Field names are case-insensitive; last duplicate fields win; null acts
as omission. Duplicate issue numbers refuse. A refusal names the line and known issue number;
known refusals say "No issues imported." Replay after an uncertain reply is safe.
