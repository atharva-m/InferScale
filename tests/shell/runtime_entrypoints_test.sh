#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
test_root="$(mktemp -d)"
trap 'rm -rf -- "$test_root"' EXIT
fake_bin="${test_root}/bin"
mkdir -p "${fake_bin}" "${test_root}/engine" "${test_root}/tokenizer"
touch "${test_root}/engine/.complete"

cat >"${fake_bin}/python3" <<'SCRIPT'
#!/usr/bin/env bash
printf '%s\n' "$@" >"${CAPTURE_FILE}"
SCRIPT
cat >"${fake_bin}/trtllm-serve" <<'SCRIPT'
#!/usr/bin/env bash
printf '%s\n' "$@" >"${CAPTURE_FILE}"
SCRIPT
chmod +x "${fake_bin}/python3" "${fake_bin}/trtllm-serve"

# Both images replace their entrypoint shell with the native server process so
# Kubernetes' post-drain SIGTERM reaches vLLM/trtllm-serve as PID 1.
grep -Fqx 'exec "${command[@]}"' "${repo_root}/runtime/vllm/entrypoint.sh"
grep -Fqx 'exec "${command[@]}"' "${repo_root}/runtime/trtllm/entrypoint.sh"

vllm_capture="${test_root}/vllm.args"
env \
  PATH="${fake_bin}:/usr/bin:/bin" \
  CAPTURE_FILE="${vllm_capture}" \
  MODEL_PATH=/cache/model \
  SERVED_MODEL_NAME=qwen-chat \
  MODEL_REVISION=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
  PRECISION=bf16 \
  QUANTIZATION=none \
  TENSOR_PARALLELISM=2 \
  MAX_MODEL_LEN=8192 \
  PREFIX_CACHING=true \
  POD_IP=10.42.0.17 \
  KV_EVENTS_ENABLED=true \
  KV_EVENTS_PORT=5557 \
  TRACING_ENABLED=false \
  PORT=8000 \
  bash "${repo_root}/runtime/vllm/entrypoint.sh"

grep -Fx -- '--tensor-parallel-size' "${vllm_capture}" >/dev/null
grep -Fx -- '2' "${vllm_capture}" >/dev/null
grep -Fx -- '--enable-prefix-caching' "${vllm_capture}" >/dev/null
grep -Fx -- '--no-enable-log-requests' "${vllm_capture}" >/dev/null
if grep -Fx -- '--disable-log-requests' "${vllm_capture}" >/dev/null; then
  echo 'vLLM argv uses the removed request logging flag' >&2
  exit 1
fi
if grep -Fx -- '--no-enable-prefix-caching' "${vllm_capture}" >/dev/null; then
  echo 'vLLM argv disables prefix caching when PREFIX_CACHING=true' >&2
  exit 1
fi
grep -Fx -- '{"enable_kv_cache_events":true,"publisher":"zmq","endpoint":"tcp://*:5557","topic":"kv@10.42.0.17:8000@qwen-chat"}' "${vllm_capture}" >/dev/null
if grep -F -- '$(POD_IP)' "${vllm_capture}" >/dev/null; then
  echo 'vLLM argv contains an unexpanded POD_IP placeholder' >&2
  exit 1
fi

env \
  PATH="${fake_bin}:/usr/bin:/bin" \
  CAPTURE_FILE="${vllm_capture}" \
  MODEL_PATH=/cache/model \
  SERVED_MODEL_NAME=qwen-fp8 \
  MODEL_REVISION=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
  PRECISION=fp8 \
  QUANTIZATION=none \
  TENSOR_PARALLELISM=1 \
  MAX_MODEL_LEN=8192 \
  PREFIX_CACHING=false \
  POD_IP=10.42.0.18 \
  KV_EVENTS_ENABLED=false \
  KV_EVENTS_PORT=5557 \
  TRACING_ENABLED=true \
  OTLP_TRACES_ENDPOINT=http://otel-collector.inferscale-monitoring.svc.cluster.local:4317 \
  PORT=8000 \
  bash "${repo_root}/runtime/vllm/entrypoint.sh"

grep -Fx -- '--dtype' "${vllm_capture}" >/dev/null
grep -Fx -- 'auto' "${vllm_capture}" >/dev/null
grep -Fx -- '--quantization' "${vllm_capture}" >/dev/null
grep -Fx -- 'fp8' "${vllm_capture}" >/dev/null
grep -Fx -- '--no-enable-prefix-caching' "${vllm_capture}" >/dev/null
if grep -Fx -- '--enable-prefix-caching' "${vllm_capture}" >/dev/null; then
  echo 'vLLM argv enables prefix caching when PREFIX_CACHING=false' >&2
  exit 1
fi
grep -Fx -- '--otlp-traces-endpoint' "${vllm_capture}" >/dev/null
grep -Fx -- 'http://otel-collector.inferscale-monitoring.svc.cluster.local:4317' "${vllm_capture}" >/dev/null
if grep -Fx -- '--kv-events-config' "${vllm_capture}" >/dev/null; then
  echo 'vLLM argv enables KV events when KV_EVENTS_ENABLED=false' >&2
  exit 1
fi

trt_capture="${test_root}/trt.args"
env \
  PATH="${fake_bin}:/usr/bin:/bin" \
  CAPTURE_FILE="${trt_capture}" \
  ENGINE_PATH="${test_root}/engine" \
  TOKENIZER_PATH="${test_root}/tokenizer" \
  TP_SIZE=4 \
  SERVED_MODEL_NAME=qwen-chat \
  MODEL_REVISION=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
  PRECISION=bf16 \
  QUANTIZATION=none \
  MAX_MODEL_LEN=8192 \
  RUNTIME_VERSION=1.0.0 \
  RUNTIME_IMAGE_DIGEST=sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd \
  GPU_ARCHITECTURE=RTX_5090 \
  ENGINE_VERIFIER="${repo_root}/runtime/trtllm/build-engine.sh" \
  PORT=8000 \
  bash "${repo_root}/runtime/trtllm/entrypoint.sh"

expected=("${test_root}/engine" --backend trt --tokenizer "${test_root}/tokenizer" --host 0.0.0.0 --port 8000 --tp_size 4)
mapfile -t actual <"${trt_capture}"
[[ "${actual[*]}" == "${expected[*]}" ]]

cat >"${fake_bin}/python3" <<'SCRIPT'
#!/usr/bin/env bash
if [[ "${1:-}" == "${TRTLLM_QWEN_CONVERTER:-}" ]]; then
  while (($#)); do
    if [[ "$1" == "--output_dir" ]]; then
      mkdir -p "$2"
      exit 0
    fi
    shift
  done
  exit 64
fi
exec /usr/bin/python3 "$@"
SCRIPT
cat >"${fake_bin}/trtllm-build" <<'SCRIPT'
#!/usr/bin/env bash
while (($#)); do
  if [[ "$1" == "--output_dir" ]]; then
    mkdir -p "$2"
    printf '{}\n' >"$2/config.json"
    exit 0
  fi
  shift
done
exit 64
SCRIPT
chmod +x "${fake_bin}/python3" "${fake_bin}/trtllm-build"
converter="${test_root}/convert_checkpoint.py"
touch "${converter}"
engine_output="${test_root}/built-engine"
build_engine() {
  env \
    PATH="${fake_bin}:/usr/bin:/bin" \
    TRTLLM_QWEN_CONVERTER="${converter}" \
    bash "${repo_root}/runtime/trtllm/build-engine.sh" \
    --model "${test_root}/tokenizer" \
    --output "${engine_output}" \
    --precision bf16 \
    --quantization none \
    --tensor-parallelism 2 \
    --max-model-len 8192 \
    --model-revision cccccccccccccccccccccccccccccccccccccccc \
    --runtime-version 1.0.0 \
    --runtime-image-digest "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd" \
    --gpu-architecture RTX_5090
}

build_engine

test -f "${engine_output}/config.json"
test -f "${engine_output}/build-metadata.json"
test -f "${engine_output}/manifest.json"
test -f "${engine_output}/.complete"
test ! -e "${test_root}/.built-engine.build-lock"

# A marker alone is insufficient: corruption of one manifest-listed file must
# quarantine the whole entry and rebuild it through atomic directory publish.
printf '{"corrupt":true}\n' >"${engine_output}/config.json"
build_engine
test "$(find "${test_root}" -maxdepth 1 -type d -name 'built-engine.corrupt.*' | wc -l)" -eq 1
/usr/bin/python3 - "${engine_output}" <<'PY'
import hashlib
import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
manifest = json.loads((root / "manifest.json").read_text())
entry = next(item for item in manifest["files"] if item["path"] == "config.json")
assert hashlib.sha256((root / "config.json").read_bytes()).hexdigest() == entry["sha256"]
assert (root / ".complete").read_text().strip() == hashlib.sha256(
    (root / "manifest.json").read_bytes()
).hexdigest()
PY

serve_built_engine() {
  env \
    PATH="${fake_bin}:/usr/bin:/bin" \
    CAPTURE_FILE="${trt_capture}" \
    ENGINE_PATH="${engine_output}" \
    TOKENIZER_PATH="${test_root}/tokenizer" \
    TP_SIZE=2 \
    SERVED_MODEL_NAME=qwen-chat \
    MODEL_REVISION=cccccccccccccccccccccccccccccccccccccccc \
    PRECISION=bf16 \
    QUANTIZATION=none \
    MAX_MODEL_LEN=8192 \
    RUNTIME_VERSION=1.0.0 \
    RUNTIME_IMAGE_DIGEST=sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd \
    GPU_ARCHITECTURE=RTX_5090 \
    ENGINE_VERIFIER="${repo_root}/runtime/trtllm/build-engine.sh" \
    PORT=8000 \
    bash "${repo_root}/runtime/trtllm/entrypoint.sh"
}

serve_built_engine
printf '{"corrupt-again":true}\n' >"${engine_output}/config.json"
if serve_built_engine 2>/dev/null; then
  echo 'TensorRT runtime accepted a post-build-corrupt engine' >&2
  exit 1
fi

echo "runtime entrypoint contract tests passed"
