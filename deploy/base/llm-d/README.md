# llm-d integration

This base installs the shared namespace and least-privilege service account. The InferScale controller creates one deployment-scoped `InferencePool`, Endpoint Picker deployment/service, and route attachment from the selected, pinned llm-d compatibility manifest. An EPP cannot be usefully deployed in this static base because its pool name and tenant namespace do not exist until an `InferenceDeployment` is reconciled.

`scripts/install-platform-dependencies.sh` installs the pinned Gateway API and Inference Extension CRDs before this base. Runtime-specific selection is not implemented in these manifests; it remains llm-d's responsibility.

The candidate matrix pins endpoint picker `v0.9.0` by multi-architecture digest for stable `InferencePool v1` plus `InferenceObjective v1alpha2`. `versions.lock.yaml` still marks conformance pending, so release/publishable use remains blocked until the required Gateway/llm-d suite records evidence.

The pinned EPP defaults to TLS on port 9002 and generates a self-signed certificate when no certificate path is supplied. InferScale explicitly sets its inference and metrics listen ports and retains TLS; port 9003 provides the separate `readiness` gRPC service. Metrics authentication is explicitly disabled to match the existing HTTP Prometheus scrape contract protected by tenant NetworkPolicies, and profiling is disabled. `EndpointPickerConfig` is a strictly decoded process configuration with `apiVersion` and `kind`, not a Kubernetes resource with `metadata`.
