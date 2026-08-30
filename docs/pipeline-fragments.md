# Pipeline fragments

One repository can execute a template defined in another, pinned to a tag.

## Authoring

A fragment is an ordinary Oberth repository with a `.oberth/fragment.yaml`
holding a Workflow document. It is a separate file from `.oberth/build.yaml`, so
one repository can both run its own pipeline and publish fragments.

```yaml
# .oberth/fragment.yaml in acme/maven-verify
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: verify
  templates:
    - name: verify
      container:
        image: golang:1.25-alpine@sha256:<digest>
        command: [/bin/sh, -c]
        args: ["./mvnw verify"]
```

## Consuming

Reference it with Argo's own `templateRef`, naming `repository@tag`:

```yaml
# .oberth/build.yaml in a consuming repository
spec:
  templates:
    - name: main
      steps:
        - - name: verify
            templateRef:
              name: acme/maven-verify@v3
              template: verify
```

The repository half is the same path used to push, `<upstream>/<repository>`.
A reference with no `@version` is refused: a fragment is always pinned.

## What the server does

Before admission, the server resolves each reference to a commit, reads
`.oberth/fragment.yaml` at that commit, inlines **all** of the fragment's
templates under generated names of the form `frag-<hash>-<template>`, and
deletes the `templateRef`. References between a fragment's own templates are
rewritten to the generated names.

The admission gate therefore sees one flat document containing everything that
will run, exactly as it does for a repository that inlined the templates by
hand. `templateRef` remains refused by the gate: a surviving one means
resolution did not happen, and the run fails rather than submitting something
the gate did not read.

## Why the ordering matters

Resolution runs before the declared-secret-path check, not merely before the
gate. A fragment's own `oberth secretstore exec --path` invocations are checked
against the consuming repository's `oberth.ci/secret-paths` annotation and its
approval-table grants, and that check only sees the fragment once its templates
are in the document.

A fragment cannot widen the secret surface. Only the consuming repository's
annotation declares paths, and annotations are not inlined. A fragment can only
spend from what the consumer already declared and was granted.

## Publishing a version

A version is a git tag, and Oberth's tag path is the release trust domain, so
publishing is not simply `git tag v3 && git push oberth`. Three things must hold:

1. The commit must be reachable from the **upstream** default branch. The
   release-admission ref tracks the upstream mirror, not Oberth's own copy, so
   with `--publish-on-green=false` a tag is never admissible until the branch
   reaches the upstream by some other route.
2. The fragment repository needs its own `.oberth/build.yaml` and a green run,
   because that is what gets the commit published in the first place.
3. Release tags should be annotated. A lightweight tag still creates the ref
   that resolution reads, but Oberth reports it as not admitted for a release
   run.

The practical consequence: a fragment is verified by its own CI before any
consumer can pin it.

## Limits and refusals

| Rule | Reason |
|---|---|
| Fragments do not nest | A fragment containing its own `templateRef` is refused. This removes cycles, depth limits and expansion bombs as a class |
| At most 16 fragments per document | Checked on the reference set before any fragment is read, so an oversized document causes no fetch |
| Only registered repositories resolve | Optionally narrowed further by `--fragment-allowlist`; unset permits every registered repository |
| A fragment is admitted like any pipeline | Inlined before the gate, so every construct `admit.go` refuses is refused inside a fragment too |
| Inlined names may not collide | With the consuming document or with another fragment |
| `spec.workflowTemplateRef` is never resolved | It names a whole spec rather than a template |

## Audit

Every submission records the fragments it used in the audit chain: repository,
requested version, resolved commit SHA, and the SHA-256 of the fragment
document. Because the record is a commit rather than a tag, moving a tag
produces a different record for the same consuming commit.

The same list is stamped on the submitted Workflow as the `oberth.ci/fragments`
annotation.

## Local validation

`oberth validate` cannot resolve fragments: it runs against a checkout with no
access to the server's git cache. It reports each reference by name and exits
non-zero rather than passing a document it did not fully check.

```
oberth validate                                # names unresolved references, exits non-zero
oberth validate --allow-unresolved-fragments   # checks the rest, fragment contents unchecked
```

On the server:

```
oberth fragments list                          # repositories publishing fragments, with versions
oberth fragments show <repository>@<version>   # the document and the commit it resolves to
```
