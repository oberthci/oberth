# Product UX requirements

Observed on the signed v0.23.4 reference (fresh and warm):

First green build: fresh needs **18 user commands / 73 flags** (6 core,
10 forced by the product, 2 for agent onboarding); warm needs **17 / 69**.
First release: fresh **30 / 97**, warm **29 / 93**. Sample authoring,
second-checkout preparation, verification reads and table-declared harness
parsing/default/timeout flags are excluded. Candidate
limits are owned by `golden-path.json`'s `candidate_target`.

- Root can write to the delivered source tree: REQ-C6-009, REQ-C6-010.
- Invalid YAML and ungranted-secret pushes succeed, then fail later: REQ-C4-011.
- A run whose entrypoint is a single step is labelled with the native name, not the authored one: REQ-C1-004.
- The CLI and the agent tools do not stream live output; only the dashboard does: REQ-C8-001, REQ-C8-002. Failed product views also lack the required next action: REQ-C5-009.

These rows describe product behavior, its owner and the assertion that closes
it. Environment and harness corrections are recorded in the walk report.
Proposals below remain proposed UX work, owner decides; they do not authorize
new commands or a second source-delivery path in this harness.

| Walk step | User sees | Should see | Owner | Walk proof / closing assertion | Decision |
| --- | --- | --- | --- | --- | --- |
| readonly-source | Signed v0.23.4 accepts the default-root probe’s first source write; ownership-only checks can conceal this. | Read-only delivery holds for the image's default root user and ordinary capabilities. | W3 / C5 execution, C6 source | `readonly-source`: real new-file, Git-file, tracked-file and chmod attempts all fail with denied access. | Required |
| invalid-yaml | Push succeeds; the later run names only walkUnknownOne on the first invalid template. | Immediate typed refusal names both offenders, file and correction. | W3 / C4 admission, C1 transport | `invalid-yaml`: `invalid_input`, both template and unknown-field names, correction, zero durable runs and native Workflows. | Required |
| ungranted-secret | A later failure names a step but does not provide the complete grant request. | Immediate `forbidden` refusal names repository, file, step and path, with the same tuple in the grant remedy. | W3 / C4 admission, C1 transport | `ungranted-secret`: exact code and tuple in reason/remedy, no run or native Workflow; unrelated refusals fail. | Required |
| install | A pre-authorized key still needs another upstream verification operation after installation. | Installation verifies the authorized key itself and prints the precise next action when it cannot. | C9 installer | `install`: all checks ready, no second `upstream add`; fewer required operations in a green measured baseline. | Required |
| install / login | Unsupported certificate requests fail without explaining the required curve; public trust uses an unexpected filename. | A supported request and the exact saved trust file/pin are explained in the result. | C9 installer / C12 client | `install` verifies the returned CA pin; failed install/enrollment must satisfy the explicit verification/redeem expectation. | Required |
| connect / onboard | Binding requires explicit certificate, key and trust options even when that client already has a saved profile. | Binding uses the selected saved connection and one consented SSH identity per checkout. | C12 client | Separate operator and invited checkouts bind once each; later commands use their saved profiles, and required binding options decrease. | Required |
| live-logs | The CLI and agent tools do not stream live output; only the dashboard does. | Plain CLI follow and active agent output expose bytes with an independently Running Pod. | C8 logs / C11 CLI | `live-logs`: CLI marker and agent bytes share one Running observation; retained-only agent output fails. | Required |
| failing-build | Failed-run views offer generic recovery text instead of the exact log command. | CLI, wait and agent results print the working command for this failed run. | C5 execution / C11 CLI | `failing-build`: authored failed step, short reason and a working log command in all three views; execute each printed command. | Required |
| ui / release-ui | A leaf entrypoint has a generated primary row label; its authored template is a subtitle. | Use the authored template as the primary label. | C1 UI / C5 execution | Compare `display_name || step || node_id` with the authored template and retain the observed dashboard objects. | Required |
| promote | A source JSON file is required, and a product report could claim that an unrelated green commit was published. | Promotion identifies its tested source, and publication is independently verifiable. | C2 actions / C11 CLI | `promote`: read forge main, fetch it, prove the pushed tested source is its ancestor and verify CI at that exact main commit. | Required |
| release | A passed run row alone can be mistaken for a recorded release. | Durable release publication names the exact tag, commit and run. | C2 actions/history | `release`: exact `release.published` event plus independent forge tag object and image-signature verification. | Required |
| grant | Storing a signing secret requires cluster access, the store root credential, a pod exec, policy sync and a full upgrade. | An ordinary operator path stores and grants the secret without these incidental operations. | C2 actions / C9 installer | `grant` still proves the exact tuple and runtime delivery; a measured green baseline drops exec/sync/upgrade operations. | Proposed UX work, owner decides |
| fixture / build / release | Source YAML needs a seven-key plugin stanza; release YAML needs projected login, public trust, memory storage, environment and a secret-client wrapper. | Authored native YAML expresses the intended build/release with less hand-written boilerplate. | C3 submission / C6 source / C13 own pipelines | `build` and `release` retain real source and grant-backed signing proofs while authored fixture declarations shrink. | Proposed UX work, owner decides |
| fixture / onboard | Users generate SSH keys and certificate requests by hand before enrollment. | Enrollment explains or performs the required local identity preparation. | C12 client | `fixture` and `onboard` preserve local-key possession and invitation redemption while required key/request commands decrease. | Proposed UX work, owner decides |
| install / onboard | Certificate installation and redemption save certificate files but leave a separate connection-profile setup to the user. | Each enrolled identity has documented, reusable connection settings. | C9 installer / C12 client / C11 CLI | Administrator upstream verification and an invited-client read succeed through saved profiles without connection flags; two manual profile actions decrease. | Required |
| grant / upgrade | Disabling backups still creates an operation record in the working directory. | Record destination is explicit and private, and output explains it. | C9 installer | `grant` selects a private record directory; final cleanup removes the private workspace after evidence is scanned. | Required |
| install | Warm endpoint verification can report EOF after deploying components. | Stable endpoint readiness precedes success; failure preserves credentials and supplies verification/resume commands. | C9 installer / C12 client | `install` endpoint check passes; a failure must include the explicit verification/resume expectation. | Required |
| target / build | A locally named source image needs registry metadata lookup; HTTP/private registries prevent startup. | Every Oberth container declares its command, including the source-plugin sidecar. | W3 / C5 execution | When the product supports its second target, the same `build` runs through real source without registry entrypoint discovery. | Required rebuild work |
| target | Local preparation has to invent plugin packaging and chart stamping. | One product build target produces paired images and their chart for release and local development. | Dev-loop / C13 own pipelines | Reintroduce the second walk target only by consuming that product output; no Dockerfile or values rewriting in the harness. | Required rebuild work |
