# Pre-rental validation record

This working record distinguishes implemented code from observed behavior.
Changes are deliberately uncommitted. A passing local functional test is not a
5090 performance result or production release sign-off.

## Observed on the local Kubernetes cluster

Hardware: one RTX 4070 Laptop GPU, 8 GiB; model Qwen3-0.6B at revision
`c1899de289a04d12100db370d81485cdf75e47ca`, BF16, TP1, context 2048.
The runtime uses the documented laptop memory/compilation overrides.

| Check | Evidence |
| --- | --- |
| Prefix caching enabled; authenticated JSON and incremental SSE | Passed through Gateway, admission and EPP. |
| Prefix reuse covering multiple router blocks | Repeating the longer prompt added **272 cache-hit tokens**. This proves runtime reuse; a single worker does not prove a routing advantage. |
| KEDA idle scale-down | The GPU worker reached zero replicas and its Pod disappeared. |
| Cold activation | Starting with no GPU worker, one inference request activated KEDA and completed in **87.505 seconds**. Model weights were already cached. No replica patch or metric injection was used. |
| Earlier lifecycle checks | Controller restart, pending candidate preservation, supersession, abort and cleanup passed in the earlier local smoke. Healthy simultaneous canaries require separate verification. |

Raw local artifacts are ignored by Git:

- `benchmarks/results/local-gpu-20260915T144443Z-b1f19918.json`: prefix checks passed;
  its combined run then stopped on a test-harness race before the autoscaler
  reconciled its minimum from one to zero. The harness now waits for that change.
- `benchmarks/results/local-gpu-20260915T194853Z-5502779c.json`: separate cold
  activation passed against the retained deployment after actual scale-down.

## Hardware-independent checks

- On September 21, the full Go race suite, vet, local control-plane vertical
  slice, generated-artifact checks and service builds passed. The real PostgreSQL
  integration suite passed again against isolated schemas, including migration
  000006 and durable operation request identities. This run includes cache-Job
  garbage collection, EPP collector wiring and admission baggage stripping.
- The current lightweight Python suite passed **65 tests** with one GuideLLM
  import skip. The benchmark suite separately passed **36 tests with GuideLLM
  0.7.0 installed**, including the previously skipped configuration test.
- GuideLLM rejected zero standard deviations. The configuration now omits them
  and keeps equal token-count bounds; the real sampler regression confirms exact
  fixed counts.
- Shell contracts, six Kustomize overlays and manifest validation passed again
  on September 21. The trace checker passed 28 offline regressions; the candidate
  dependency bundle passed eight offline tests and a complete render from
  checksum-verified official manifests. Ruff and mypy passed on project sources.
- The cluster's pinned `promtool` accepted all **31 recording/alert rules**.
- The weighted Gateway addon passed its full extension-server package tests and
  controller compilation. Envoy Gateway translation changes, the cyclic EPP
  picker and their live integration are still being validated.

## Remaining local release work

- Finish and test the compatible Gateway/addon/EPP dependency images, then run
  healthy weighted-pool, shadowing, spoofed-header, queue and cancellation checks.
- Prove one caller trace spans Gateway, admission, EPP and runtime, and inspect
  exported attributes/events for request-content or credential leakage.
- Recheck all code after the final fixes, including cache-Job Pod collection,
  EPP collector configuration and TRT wrapper changes.
- Integrate qualified dependency identities into the reproducible installer and
  release instructions. Keep the release conformance gate pending until its
  actual required evidence exists.

## Hardware-specific acceptance still required

Real TP2/TP4 execution and communication behavior, multi-worker GPU scaling,
5090 TensorRT engine compatibility/parity and performance measurements require
matching hardware. Native CPU fixtures can validate routing/control behavior,
but cannot substitute for these GPU results. Do not rent hardware merely to
work around unfinished software integration.

## Local restart and resource limits

The September 16 restart was identified in Windows event logs as a planned
Windows Update restart (`MoUsoCoreWorker.exe`, then `TrustedInstaller.exe`),
although it was unexpected by the operator. This does not establish the cause
of earlier incidents. Docker Desktop and the existing k3d cluster were restored.
Builds run serially in a dedicated builder limited to two CPUs and 4 GiB RAM,
with total memory plus swap also capped at 4 GiB. The GPU worker remains at zero
while dependency builds run. Logs and source inputs now persist under ignored
`benchmarks/artifacts/v1-validation/`, rather than restart-volatile `/tmp`.
