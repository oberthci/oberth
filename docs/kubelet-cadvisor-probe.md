# Read-only tuxbox cAdvisor format probe

The appended Opus R2 review supplied by the coordinator reports **format PASS**
on k3s 1.36.5 from two read-only API-proxy GETs using its existing kubeconfig
(without sudo). Workers did not access tuxbox.

| Family | Total | Main | Main with id | Main with timestamp | Newest age |
| --- | ---: | ---: | ---: | ---: | ---: |
| `container_memory_usage_bytes` | 79 | 5 | 5 | 5 | 2.4s |
| `container_memory_max_usage_bytes` | 79 | 5 | 5 | 5 | 2.4s |
| `container_cpu_usage_seconds_total` | 78 | 5 | 5 | 5 | 2.4s |

The second scrape reported five nonzero main high-water values, five id paths
containing a Pod UID/full 64-hex container ID, and 60 main filesystem-counter
lines. This shape check is looser than `CgroupMatches`; the direct-kubelet
TLS/SAN/token/stats-metrics RBAC route and PSI/ephemeral summary fields remain
unqualified. The command below is available to repeat the format check.

Run during a representative CI main container. This prints only counts and
source ages, never raw labels, names, IDs, tokens or environment values:

```sh
(set -o pipefail; sudo k3s kubectl get --raw /api/v1/nodes/tuxbox/proxy/metrics/cadvisor | awk -v now="$(date +%s)" 'BEGIN{split("container_memory_usage_bytes container_memory_max_usage_bytes container_cpu_usage_seconds_total",names," ")} /^container_(memory_usage_bytes|memory_max_usage_bytes|cpu_usage_seconds_total)[{]/{n=$0;sub(/[{].*/,"",n);total[n]++;if(!index($0,"container=\"main\""))next;main[n]++;if(index($0,"{id=\"/")||index($0,",id=\"/"))ids[n]++;if($0~/[}][[:space:]]+[^[:space:]]+[[:space:]]+[0-9]+[[:space:]]*$/){stamped[n]++;if($NF>newest[n])newest[n]=$NF}} END{for(i=1;i<=3;i++){n=names[i];printf "%s total=%d main=%d main_with_id=%d main_with_timestamp=%d newest_main_age_seconds=%s\n",n,total[n],main[n],ids[n],stamped[n],stamped[n]?sprintf("%.1f",now-newest[n]/1000):"unknown"}}')
```

Main samples require both a cgroup `id` path and an actual trailing Unix-ms
source timestamp; compare counts and ages (sampler: 30s old / 2s ahead maximum).
Missing high-water metrics fall back to current usage. No main samples means
repeat while a step is active, not a qualification pass.

This read-only host-admin API proxy probe does not grant the server nodes/proxy
and does not prove its direct-kubelet TLS, SAN, token audience or stats/metrics
RBAC path. Qualify that separately before enabling the opt-in 15s sampler.

When the sampler is enabled, a failed or partial node scrape emits one
`WARN: external resource sampler node="..." scrape incomplete` per node outage,
with `summary_ok` and `metrics_ok` fields. TLS/SAN, token/RBAC, HTTP, response
bound, parse and shared-gate timeout failures all leave the affected data
unknown; no raw endpoint, token, response body or error detail is logged.
The existing unscoped-admin `/metrics` endpoint exposes
`oberth_resource_sampler_scrape_failures_total` (one increment per completed
shared fill missing either endpoint, not per run/waiter) and
`oberth_resource_sampler_nodes_unavailable` (nodes whose last attempted fill
was incomplete). Both reset on server restart; unattempted nodes are not
counted unavailable. Metrics reads do not trigger scrapes or query storage.
Cached failed snapshots do not increment the counter. Failures continue to be
retried on the sampling cadence without repeating the warning. Both endpoints
must succeed to log `scrape recovered`, clear that node's unavailable state
and re-arm its next outage warning. Partial success remains usable.

During qualification, check the warning/counter and actual fresh usage rows;
an unavailable scrape or unsampled result is not evidence of low resource use.
These diagnostics never change run results, readiness or admission, and the
metrics are absent when no sampler is configured. They establish endpoint
fetch/parse health only: successful scrapes alone do not prove timestamp,
UID/container binding or workload coverage. Confirm those separately above.
