# Runtime images

InferScale wraps upstream runtime images only to provide a validated, injection-safe environment contract. Runtime behavior remains owned by vLLM and TensorRT-LLM.

The checked-in defaults are the digest-pinned candidates in `versions.lock.yaml`. `scripts/build-release-images.sh` validates any override as an immutable digest before building release outputs:

```bash
docker build --build-arg VLLM_IMAGE=vllm/vllm-openai@sha256:<digest> -t inferscale-vllm runtime/vllm
docker build --build-arg TRTLLM_IMAGE=nvcr.io/nvidia/tensorrt-llm/release@sha256:<digest> -t inferscale-trtllm runtime/trtllm
```

vLLM's controller contract is `MODEL_PATH`, `SERVED_MODEL_NAME`, immutable `MODEL_REVISION`, `PRECISION`, `QUANTIZATION`, `TENSOR_PARALLELISM`, `MAX_MODEL_LEN`, `PREFIX_CACHING`, `POD_IP`, `KV_EVENTS_ENABLED`, `KV_EVENTS_PORT`, and `TRACING_ENABLED`. When enabled, the entrypoint constructs `--kv-events-config` inside the container with endpoint `tcp://*:<port>` and topic `kv@<pod-ip>:<http-port>@<served-model>`; Kubernetes-style `$(POD_IP)` placeholders can never leak into vLLM argv. Tracing additionally requires `OTLP_TRACES_ENDPOINT` and passes only the upstream server's supported OTLP endpoint flag. Arbitrary extra arguments are deliberately unsupported.

TensorRT-LLM v1 accepts `ENGINE_PATH`, `TOKENIZER_PATH`, `TP_SIZE`, `SERVED_MODEL_NAME`, immutable `MODEL_REVISION`, and `PORT`. It serves a prebuilt engine with the corresponding tokenizer. The engine cache key covers model revision, complete runtime-image identity, precision/quantization, TP, maximum context, and GPU architecture. A build is produced in a sibling staging directory, populated with identity metadata and a manifest of every file's size/SHA-256, fully re-verified, and atomically renamed into place. `.complete` contains the manifest digest and is never sufficient by itself. A corrupt or incompatible entry is atomically quarantined and rebuilt; it is never repaired file-by-file. Every worker repeats the full identity, manifest, inventory, size, and hash verification before `trtllm-serve` starts, so a marker-only or post-build-corrupt entry is never mounted as a usable engine.

The pinned TensorRT-LLM 1.0 server returns JSON iteration statistics from `/metrics`, not Prometheus text, and polling drains its internal statistics queue. Its pod therefore includes one `metrics_exporter.py` sidecar as the sole upstream poller; Prometheus scrapes the sidecar rather than the runtime endpoint. The exporter publishes only observed running/waiting, memory, and KV-cache gauges on port 9000. It deliberately does not infer request totals, token totals, TTFT, or TPOT. llm-d EPP measures those request/stream signals uniformly for both backends, while external benchmark measurements remain authoritative for backend-selection profiles. If the pinned full-duplex EPP integration does not expose that evidence, progressive rollout pauses and a benchmark profile remains ineligible rather than substituting values. This limitation cannot be bypassed by the feature gate or by treating a PyTorch backend as a TensorRT engine.

Both worker adapters use the same Kubernetes-native shutdown contract. Deleting
a Pod makes its EndpointSlice entry `ready=false` and `terminating=true`; a
15-second `preStop.sleep` action gives the endpoint picker and data plane time to
observe that state while established streams remain connected. Kubernetes then
sends the native runtime process `SIGTERM` (both entrypoints use `exec`) and lets
it finish active streams within the remainder of a 120-second Pod termination
grace period. The grace period bounds shutdown; no InferScale drain proxy or
image-provided shell hook is involved.
