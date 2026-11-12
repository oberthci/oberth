# Host preparation

`oberth install plan` reports advisory checks for swap or zram, inotify limits,
and kubelet reservations and memory eviction. Warnings do not block the plan. JSON checks mark advisory failures with
`warning: true`; installation readiness and pending-key results ignore them.
The installer reads local procfs only when its Linux hostname identifies the
selected node. For remote nodes it reports unverified settings with the same
remedies; it never inspects the client kernel as evidence about another host.
Kubelet settings are read through the selected node's authenticated `configz`
proxy. Missing permission or unavailable data produces a warning.

## Swap or zram

Install `zram-tools` on the node, set `PERCENT=25` in
`/etc/default/zramswap`, then run:

```sh
sudo systemctl enable --now zramswap
cat /proc/swaps
```

Set kubelet `failSwapOn: false` and `memorySwap.swapBehavior: NoSwap` to leave
swap available for host daemons while pipeline Pods remain unswapped. Follow
your distribution's zram package instructions where `zram-tools` is unavailable.

## Inotify

Write `/etc/sysctl.d/90-oberth.conf`:

```ini
fs.inotify.max_user_instances=1024
fs.inotify.max_user_watches=1048576
```

Run `sudo sysctl --system` and verify both values with
`sysctl fs.inotify.max_user_instances fs.inotify.max_user_watches`.

## Kubelet

Set these fields in the node's `KubeletConfiguration`, then restart kubelet:

```yaml
systemReserved:
  cpu: 500m
  memory: 1Gi
evictionHard:
  memory.available: 500Mi
  nodefs.available: 10%
  imagefs.available: 15%
```

For k3s, merge the equivalent arguments into `/etc/rancher/k3s/config.yaml`:

```yaml
kubelet-arg:
  - "system-reserved=cpu=500m,memory=1Gi"
  - "eviction-hard=memory.available<500Mi,nodefs.available<10%,imagefs.available<15%"
```

Then run `sudo systemctl restart k3s`. Preserve any existing kubelet arguments.
These reservations reduce node allocatable capacity; Oberth's CPU headroom is
applied to the resulting allocatable figure. Plan maintenance before restarting
a node's kubelet or k3s service.
