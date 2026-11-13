# Run facts

Oberth sets these native Argo Workflow parameters in
`spec.arguments.parameters` on every compiled run:

| Parameter | Value |
| --- | --- |
| `oberth-repo` | Run repository name |
| `oberth-ref` | Exact run ref |
| `oberth-sha` | Exact run commit SHA |
| `oberth-trigger` | Durable event: `branch`, `promotion`, `schedule` or `tag` |
| `oberth-release-tag` | Exact ref, on release-tier runs only |
| `oberth-release-sha` | Exact SHA, on release-tier runs only |

Release parameters are absent on CI runs, including ordinary tags. Release
runs require a nonempty ref in the allowed alphabet and a complete commit SHA;
Build refuses a missing fact rather than submitting an empty release parameter.
Oberth does not infer a missing value from another fact.
The trigger describes the event, independently of the CI/release trust tier;
release-tier runs have trigger `tag`.

The run name is available as `{{workflow.name}}`. For scheduled runs,
`oberth-schedule` identifies the resolved schedule entry as described in
[scheduled runs](scheduled-runs.md).

The entire `oberth-` parameter prefix is reserved for the server. Authors may
reference these names without declaring them. Authored declarations are refused
in workflow arguments, template inputs/outputs and task/step/hook arguments,
including bare names, `value`, `valueFrom`, `default`, `enum` and output
`globalName: oberth-*`. Rename any
repository-defined parameter using this prefix. A mutex or template name with
this prefix is unaffected.

Oberth no longer injects `OBERTH_REPO`, `OBERTH_REF`, `OBERTH_SHA`,
`OBERTH_TRIGGER`, `OBERTH_RUN_ID`, `OBERTH_RELEASE_TAG`, `OBERTH_RELEASE_SHA` or
`OBERTH_STEP`. Map facts to whatever environment names your commands require.
For example, in a release-only container:

```yaml
env:
  - {name: OBERTH_REPO, value: "{{workflow.parameters.oberth-repo}}"}
  - {name: OBERTH_REF, value: "{{workflow.parameters.oberth-ref}}"}
  - {name: OBERTH_SHA, value: "{{workflow.parameters.oberth-sha}}"}
  - {name: OBERTH_TRIGGER, value: "{{workflow.parameters.oberth-trigger}}"}
  - {name: OBERTH_RELEASE_TAG, value: "{{workflow.parameters.oberth-release-tag}}"}
  - {name: OBERTH_RELEASE_SHA, value: "{{workflow.parameters.oberth-release-sha}}"}
```

Use only the first four mappings on CI containers. The `OBERTH_TRIGGER` mapping
now yields the event vocabulary above; an author who needs the trust tier can
set it explicitly in each pipeline. Map `OBERTH_RUN_ID` to `{{workflow.name}}`
if needed. An authored `OBERTH_SHA` or other run-fact env value is preserved;
the server-owned Workflow parameter remains independent of it. Pass parameters
through env rather than interpolating untrusted refs directly into shell code.

Live mechanism variables remain: `OBERTH_TOOLS_DIR` and `OBERTH_CACHE_DIR`
(toolchain/workspace), `OBERTH_ARTIFACTS` (artifact collector),
`OBERTH_ASSIST_HOLD` (assisted holds), and `OBERTH_VAULT_ROLE` /
`OBERTH_RELEASE_WIF` (secret-store/grant delivery). These are separate from
run-fact injection. Toolchain/workspace and collector removal belongs to the
v0.23.0 work; secret-store env belongs to S6/T3.

The allowed alphabet for refs, branch names and tags is
`^[A-Za-z0-9][A-Za-z0-9._/+@=,-]*$`. Existing Git structural restrictions
(such as `..` and `.lock`) also apply at receive. Both receive and Workflow
compilation refuse characters outside this alphabet with the same plain
message. An already-stored or mirrored run ref outside it is refused at Build;
it is never passed through to an Argo parameter. Version-tag detection is
anchored at both ends and its suffix uses the same alphabet.

Never use `oberth-ref` or `oberth-release-tag` in `when:`. Use the closed
`oberth-trigger` vocabulary to select work by event, and pass ref/tag facts
through env to commands. Assisted hold step selection always uses the Argo
node name; an authored `OBERTH_STEP` cannot change it.

Before upgrading, an operator can list names outside this alphabet by running
this read-only command once for each hosted bare repository:

```sh
git -C /path/to/hosted.git for-each-ref --format='%(refname)' refs/heads/ refs/tags/ | rg -v '^refs/(heads|tags)/[A-Za-z0-9][A-Za-z0-9._/+@=,-]*$'
```

No output means there are no such names (`rg` exits 1 for no matches). These
branches and tags are not silently dropped: cache storage and refresh preserve
their Git names. Their next push or run is refused with:

```text
ref name is invalid: start with a letter or digit; use only letters, digits and . _ / + @ = , -
```

Rename an unsupported branch or tag explicitly before submitting it again.
