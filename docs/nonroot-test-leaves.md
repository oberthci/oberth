# Authored nonroot execution

Use native Kubernetes `securityContext` fields on an Argo Workflow or template.
Oberth passes authored Pod and container contexts through unchanged. The removed
`oberth.ci/nonroot-templates` annotation is refused; remove it and declare the
user, groups and storage the container actually needs. No preparation container,
security context or private `/tmp` volume is injected into pipeline templates.

For example, a template can select an ordinary nonroot user and private scratch:

```yaml
- name: test
  securityContext:
    runAsUser: 65534
    runAsGroup: 65534
    runAsNonRoot: true
    fsGroup: 65534
    seccompProfile:
      type: RuntimeDefault
  volumes:
  - name: private-tmp
    emptyDir:
      sizeLimit: 512Mi
  container:
    image: your-approved-image
    securityContext:
      allowPrivilegeEscalation: false
      readOnlyRootFilesystem: true
      capabilities:
        drop: [ALL]
    volumeMounts:
    - name: private-tmp
      mountPath: /tmp
```

Choose volume ownership and paths compatible with the image and Kubernetes
storage. Explicit workspace annotations still control shared mounts and cache
access; selecting a user does not change those choices. See
[workspace isolation](workspace-isolation.md). Author any required scratch volume
and size limit explicitly. Oberth does not grant capabilities or prepare paths
to make a particular user work.

Kubernetes enforces the Pod security floor on every container kind. Privileged
containers, added capabilities other than NET_BIND_SERVICE, missing drop ALL,
privilege escalation and unsafe effective seccomp are refused at Pod creation.
Step Pods need RuntimeDefault or Localhost seccomp; Pod-level Unconfined is
refused even when every container supplies a safe override. Root execution and
writable root filesystems are allowed. Argo supplies native main/executor
security defaults configured by the installer. Author-provided init containers
and sidecars must meet the same floor themselves.

Go admission still refuses authored `appArmorProfile`, `seLinuxOptions`,
`procMount`, `sysctls`, `windowsOptions` and beta AppArmor Pod annotations.
Other security-context fields pass through, including unsafe values that the
Kubernetes policy later refuses. Host namespaces, host paths, host ports,
identity overrides and unauthorized secret access keep their separate refusals.

The executor-token delivery waiter keeps its existing security context and token
relocation. Argo's native wait-container credential mirrors are retained;
source and explicit read-only workspace mirrors retain their protections. The
waiter keeps the executor token at a random path in private memory-backed
storage, with no token at the fixed default path. This protection is independent
of the removed wait-container secret-mount deletion patch.
