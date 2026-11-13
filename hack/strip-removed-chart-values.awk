# Print a chart values file without the values the chart removed (the list in
# internal/installer/upgrade_removed_values.go). The historical fixtures keep
# them; the target schema refuses them, and an upgrade refuses stored ones.
BEGIN {
  split("admission autoRetryInfrastructureBudget argo.ciSecretsServiceAccount argo.ciServiceAccount argo.containerDiskPrefixes argo.enabled argo.defaultWorkflowTimeout argo.repoWorkflowTimeouts argo.resourceSampler argo.workflowTimeout argo.kubevirtCeiling argo.vault.ciSecretsRole argo.vmLauncherServiceAccount argoSync auditAnchor.rootsKey gitBackend gitcache maxConcurrentJobs platformProtection releases secretCreatePolicy trustedTests upstream.createSecrets upstream.keySecret upstream.knownHostsSecret vm watchTunnel", list, " ")
  for (i in list) removed[list[i]] = 1
}
function indentation(line) { match(line, /^ */); return RLENGTH }
{
  if ($0 ~ /^[[:space:]]*(#.*)?$/) { if (!skipping) print; next }
  depth = indentation($0)
  if (skipping && depth > skip_depth) next
  skipping = 0
  key = $1
  sub(/:.*/, "", key)
  level = depth / 2
  path[level] = key
  dotted = path[0]
  for (i = 1; i <= level; i++) dotted = dotted "." path[i]
  if (dotted in removed) {
    skipping = 1
    skip_depth = depth
    next
  }
  print
}
