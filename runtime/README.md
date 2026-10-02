# Runtime images

InferScale wraps upstream runtime images only to provide a validated, injection-safe environment contract. Runtime behavior remains owned by vLLM and TensorRT-LLM.

The checked-in defaults are the digest-pinned candidates in `versions.lock.yaml`. `scripts/build-release-images.sh` validates any override as an immutable digest before building release outputs:

```bash
docker build --build-arg VLLM_IMAGE=vllm/vllm-openai@sha256:<digest> -t inferscale-vllm runtime/vllm
docker build --build-arg TRTLLM_IMAGE=nvcr.io/nvidia/tensorrt-llm/release@sha256:<digest> -t inferscale-trtllm runtime/trtllm
```

vLLM's controller contract is `MODEL_PATH`, `SERVED_MODEL_NAME`, immutable `MODEL_REVISION`, `PRECISION`, `QUANTIZATION`, `TENSOR_PARALLELISM`, `MAX_MODEL_LEN`, `PREFIX_CACHING`, `POD_IP`, `KV_EVENTS_ENABLED`, `KV_EVENTS_PORT`, and `TRACING_ENABLED`. When enabled, the entrypoint constructs `--kv-events-config` inside the container with endpoint `tcp://*:<port>` and topic `kv@<pod-ip>:<http-port>@<served-model>`; Kubernetes-style `$(POD_IP)` placeholders can never leak into vLLM argv. Tracing additionally requires `OTLP_TRACES_ENDPOINT` and passes only the upstream server's supported OTLP endpoint flag. Arbitrary extra arguments are deliberately unsupported.

TensorRT-LLM v1 accepts `ENGINE_PATH`, `TOKENIZER_PATH`, `TP_SIZE`, `SERVED_MODEL_NAME`, immutable `MODEL_REVISION`, `MAX_MODEL_LEN`, `PREFIX_CACHING`, and `PORT`, plus the engine identity inputs below. It serves a prebuilt engine with the corresponding tokenizer. The engine cache key covers model revision, complete runtime-image identity, precision/quantization, TP, maximum context, and GPU architecture. A build is produced in a sibling staging directory, populated with identity metadata and a manifest of every file's size/SHA-256, fully re-verified, and atomically renamed into place. `.complete` contains the manifest digest and is never sufficient by itself. A corrupt or incompatible entry is atomically quarantined and rebuilt; it is never repaired file-by-file. Every worker repeats the full identity, manifest, inventory, size, and hash verification before `trtllm-serve` starts, so a marker-only or post-build-corrupt entry is never mounted as a usable engine.

The pinned [1.0.0 CLI](https://github.com/NVIDIA/TensorRT-LLM/blob/v1.0.0/tensorrt_llm/commands/serve.py) has no served-model-name flag. Its [OpenAI server](https://github.com/NVIDIA/TensorRT-LLM/blob/v1.0.0/tensorrt_llm/serve/openai_server.py) derives the public model name from the engine directory basename. The entrypoint therefore creates a private temporary symlink named after the deployment and passes that path to the native server; the verified engine cache remains unchanged. It passes the requested context limit as `--max_seq_len`. It also supplies `kv_cache_config.enable_block_reuse` through `--extra_llm_api_options` for both cache-on and cache-off deployments. This is necessary because the pinned [KV cache configuration](https://github.com/NVIDIA/TensorRT-LLM/blob/v1.0.0/tensorrt_llm/llmapi/llm_args.py) defaults block reuse to enabled. Prefix caching within each TRT worker is supported; precise prefix-aware routing still requires a verified llm-d KV-event integration and is rejected by the TRT adapter.

The pinned TensorRT-LLM 1.0 server returns JSON iteration statistics from `/metrics`, not Prometheus text, and polling drains its internal statistics queue. Its pod therefore includes one `metrics_exporter.py` sidecar as the sole upstream poller; Prometheus scrapes the sidecar rather than the runtime endpoint. The exporter publishes only observed running/waiting, memory, and KV-cache gauges on port 9000, including the native `kvCacheStats.cacheHitRate` when present and valid. The normalized prefix-cache ratio uses this [upstream block reuse ratio](https://github.com/NVIDIA/TensorRT-LLM/blob/v1.0.0/cpp/include/tensorrt_llm/executor/types.h); it measures reused blocks divided by reused plus missed blocks, whereas vLLM's ratio measures tokens. Missing or invalid cache evidence remains absent and cannot qualify a cache-enabled benchmark. The exporter deliberately does not infer request totals, token totals, TTFT, or TPOT. llm-d EPP measures those request/stream signals uniformly for both backends, while external benchmark measurements remain authoritative for backend-selection profiles. If the pinned full-duplex EPP integration does not expose that evidence, progressive rollout pauses and a benchmark profile remains ineligible rather than substituting values. This limitation cannot be bypassed by the feature gate or by treating a PyTorch backend as a TensorRT engine.

Both worker adapters use the same Kubernetes-native shutdown contract. Deleting
a Pod makes its EndpointSlice entry `ready=false` and `terminating=true`; a
15-second `preStop.sleep` action gives the endpoint picker and data plane time to
observe that state while established streams remain connected. Kubernetes then
sends the native runtime process `SIGTERM` (both entrypoints use `exec`) and lets
it finish active streams within the remainder of a 120-second Pod termination
grace period. The grace period bounds shutdown; no InferScale drain proxy or
image-provided shell hook is involved.

GPU workers and TensorRT engine builders mount a private, memory-backed
`emptyDir` at `/dev/shm` for communication between GPU worker processes. Its
upper bound is 2 GiB per allocated GPU: 2 GiB for TP1, 4 GiB for TP2, and 8 GiB
for TP4. It uses host RAM as pages are written, not GPU VRAM, and does not reserve
the entire bound at startup. Include this memory in host capacity planning;
validate the bound with the selected model and concurrency during qualification.
The Pods do not enable host IPC. This follows the [vLLM Kubernetes deployment
guidance](https://docs.vllm.ai/en/latest/deployment/k8s/).

V1 places each replica and its complete model/engine cache on one selected GPU
host. The configured GPU inventory must select that host by
`kubernetes.io/hostname`, in addition to the GPU product label. All GPUs used by
one replica must be on that machine. An eight-GPU machine can run eight TP1,
four TP2, or two TP4 replicas before reserving room for rolling updates and other
deployments; TP8 is not part of the v1 contract. Pointing the same SKU selector at
multiple GPU machines does not replicate the node-local cache and is unsupported.
