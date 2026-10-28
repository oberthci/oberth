# Argo dependency expansion

`pkg/argoworkflow/vmi_depends_expand.go` is an adapted, locally vendored copy of
`GetTaskDependencies`, `getTaskDependsLogic`, `expandDependency` and
`taskNameRegex` from Argo Workflows **v4.0.8**, `workflow/common/ancestry.go`.
Upstream: https://github.com/argoproj/argo-workflows/blob/v4.0.8/workflow/common/ancestry.go

SHA256 of that upstream source file:
`02e91b1249ae850c5f2d63d2e27ec15b637d28a16fe3e3e3be341efaf03bf262`.
Copyright 2017-2018 The Argo Authors; Apache-2.0, retained in [LICENSE](LICENSE).
The upstream module contains no NOTICE file.

Oberth changes names and the lookup interface, returns just the dependency-name
set (item-vs-task classification is not needed), and refuses nil/missing tasks
instead of dereferencing them. Expansion order, regex boundaries and generated
boolean text remain unchanged. The wrapper still checks the closed grammar,
normalizes hyphens and compiles a typed eight-boolean TaskResults environment;
it never executes repository expressions. ContinueOn expansion is retained for
parity even though the VMI projection refuses ContinueOn workflows.

`TestVMIArgoDependencyExpansionParity` compares exact expanded strings and
referenced names against the pinned Argo implementation, imported only by tests.
Missing/nil references have separate refusal tests because upstream panics on
some of them. Keep this copy and its parity tests synchronized when updating
Argo. This removes the workflow/common production import and its transitive
packages without changing the dependency versions or submitted workflow bytes.
