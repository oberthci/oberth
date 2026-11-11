# Print a chart values file without the values the chart removed (the list in
# internal/installer/upgrade_removed_values.go). The historical fixtures keep
# them; the target schema refuses them, and an upgrade refuses stored ones.
function indentation(line) { match(line, /^ */); return RLENGTH }
{
  if ($0 ~ /^[[:space:]]*(#.*)?$/) { if (!skipping) print; next }
  depth = indentation($0)
  if (skipping && depth > skip_depth) next
  skipping = 0
  key = $1
  sub(/:.*/, "", key)
  if (depth == 0) parent = key
  if (depth == 0 && key ~ /^(argoSync|gitBackend|gitcache|maxConcurrentJobs|trustedTests|vm)$/ ||
      depth == 2 && parent == "admission" && key ~ /^(declaredResources|exclusiveClasses|legacyResources|maxHeadWait|runtimeCapacity|sizeWeights)$/ ||
      depth == 2 && parent == "argo" && key ~ /^(containerDiskPrefixes|kubevirtCeiling|vmLauncherServiceAccount)$/) {
    skipping = 1
    skip_depth = depth
    next
  }
  print
}
