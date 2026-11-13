# Generated helper Pod validation

The test exports current seed, collect and executor-identity Pods and runs this
isolated, pinned Kubernetes v1.35.7 validator. It applies native Pod defaults,
executes the real Priority admission plugin with no operator default class, and
runs ValidatePodCreate on each admitted Pod. Reintroducing PreemptNever without
a PriorityClass must fail Priority admission for every helper shape.

The module starts from the existing isolation fixture validator's Kubernetes
v1.35.7 pins; its dependency lock also includes Priority admission and fake-client
imports. The root production module does not gain a Kubernetes server dependency.
Historical controller Pod captures and their provenance remain unchanged.
