Shared model-cache verification runs in `inferscale-model-cache`. Its namespace
permits hostPath volumes because each verifier hashes immutable bytes on the
observed GPU node. Control-plane Pod Security enforcement remains restricted.
Verifier Pods run as UID/GID 65532 with a read-only root filesystem and cache
mount, dropped capabilities, default seccomp, and a bounded writable `/tmp`.
Each verifier requests 100m CPU/128Mi memory and is limited to 500m CPU/512Mi
memory. Its 30-minute deadline and zero retries remain in effect.

The namespace denies all Pod ingress and egress. Verification reads local files
and does not download models or contact the Kubernetes API. Download and repair
Jobs remain in tenant namespaces. If `INFERSCALE_IMAGE_PULL_SECRET` is configured,
provision that registry Secret in this namespace as well; image pulls are
performed by the node and do not require Pod egress.

When upgrading from control-namespace verification, install this namespace and
policy before restarting the controller. The controller schedules fresh shared
proofs here and no longer consumes shared Jobs in `inferscale-system`. Preserve
old Job and Pod evidence, then remove only those old shared verifier Jobs after
their replacement has completed full verification successfully. These Jobs
intentionally have no TTL so checksum failure evidence cannot expire unseen;
they will otherwise remain until explicitly removed. Do not remove repair
Leases or prefetch Jobs as part of that namespace cleanup.
