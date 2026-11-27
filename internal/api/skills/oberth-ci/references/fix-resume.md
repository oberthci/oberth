# Fix and resume: shipped retry mode

If a long run failed late: fix, commit, after consent push without oberth.run, then run_resume the failed run with that commit.
On run.finished with resumed_from set: if it passed, after consent run_rerun it once; if it failed, fix and resume again.
Never promote a resumed run. An assisted run is never evidence. Promote only a passed unassisted run of that exact commit.

The only shipped response mode is `mode:"retry"`. There is no same-Pod resume mode.
Serial flow: failed R1 -> consented committed parked F -> explicit run_resume -> Assisted R2 -> consented run_rerun R2 -> fresh unassisted RF.
Use `run_resume {"id":"11111111","commit":"2222222222222222222222222222222222222222"}` only after the fix is committed and pushed parked on that branch.
This retries the failed part through Argo while retaining passed work and carried state; it never turns R2 into evidence.
Observe the exact resumed run receipt, independently of R1. If R2 fails, fix and explicitly resume again.
After R2 passes, obtain consent for one fresh full unassisted run and use `run_rerun {"id":"22222222"}` on R2.
Do not request RF without that consent. A prior authorization for this exact action needs no second prompt.
Only RF's passed unassisted exact commit can be promoted. Confirm delivery through publication, not merely the test pass.
Changed pipelines or grants require a full pushed run. Carried caches and external effects are not fresh evidence.
Same-Pod behavior remains gated separately and must not be inferred from this retry workflow.
