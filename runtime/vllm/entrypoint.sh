#!/usr/bin/env bash
set -euo pipefail

required=(
  MODEL_PATH
  SERVED_MODEL_NAME
  MODEL_REVISION
  PRECISION
  QUANTIZATION
  TENSOR_PARALLELISM
  MAX_MODEL_LEN
  PREFIX_CACHING
  POD_IP
  KV_EVENTS_ENABLED
  KV_EVENTS_PORT
  TRACING_ENABLED
)
for variable in "${required[@]}"; do
  if [[ -z "${!variable:-}" ]]; then
    echo "missing required environment variable: ${variable}" >&2
    exit 64
  fi
done

if [[ ! "${MODEL_REVISION}" =~ ^[0-9a-fA-F]{40}$ ]]; then
  echo "MODEL_REVISION must be an immutable 40-character commit SHA" >&2
  exit 64
fi

case "${PRECISION}" in
  bf16) dtype=bfloat16 ;;
  fp8) dtype=auto ;;
  fp16) dtype=float16 ;;
  fp32) dtype=float32 ;;
  auto) dtype=auto ;;
  *) echo "unsupported PRECISION: ${PRECISION}" >&2; exit 64 ;;
esac

if [[ ! "${TENSOR_PARALLELISM}" =~ ^[1-9][0-9]*$ ]]; then
  echo "TENSOR_PARALLELISM must be a positive integer" >&2
  exit 64
fi
if [[ ! "${MAX_MODEL_LEN}" =~ ^[1-9][0-9]*$ ]]; then
  echo "MAX_MODEL_LEN must be a positive integer" >&2
  exit 64
fi
runtime_port="${PORT:-8000}"
if [[ ! "${runtime_port}" =~ ^[0-9]+$ ]] || (( runtime_port < 1 || runtime_port > 65535 )); then
  echo "PORT must be in the range 1-65535" >&2
  exit 64
fi
if [[ ! "${KV_EVENTS_PORT}" =~ ^[0-9]+$ ]] || (( KV_EVENTS_PORT < 1 || KV_EVENTS_PORT > 65535 )); then
  echo "KV_EVENTS_PORT must be in the range 1-65535" >&2
  exit 64
fi
if [[ ! "${POD_IP}" =~ ^[0-9A-Fa-f:.]+$ ]]; then
  echo "POD_IP contains invalid characters" >&2
  exit 64
fi
if [[ ! "${SERVED_MODEL_NAME}" =~ ^[A-Za-z0-9][A-Za-z0-9._:/-]*$ ]]; then
  echo "SERVED_MODEL_NAME contains characters that cannot be encoded in the KV topic" >&2
  exit 64
fi

command=(
  python3 -m vllm.entrypoints.openai.api_server
  --host "${HOST:-0.0.0.0}"
  --port "${runtime_port}"
  --model "${MODEL_PATH}"
  --served-model-name "${SERVED_MODEL_NAME}"
  --dtype "${dtype}"
  --tensor-parallel-size "${TENSOR_PARALLELISM}"
  --max-model-len "${MAX_MODEL_LEN}"
  --gpu-memory-utilization "${GPU_MEMORY_UTILIZATION:-0.90}"
  --no-enable-log-requests
)

if [[ "${PRECISION}" == "fp8" ]]; then
  if [[ "${QUANTIZATION}" != "none" && "${QUANTIZATION}" != "fp8" ]]; then
    echo "PRECISION=fp8 cannot be combined with QUANTIZATION=${QUANTIZATION}" >&2
    exit 64
  fi
  command+=(--quantization fp8)
elif [[ "${QUANTIZATION}" != "none" ]]; then
  case "${QUANTIZATION}" in
    awq|gptq|fp8) command+=(--quantization "${QUANTIZATION}") ;;
    *) echo "unsupported QUANTIZATION: ${QUANTIZATION}" >&2; exit 64 ;;
  esac
fi

if [[ "${PREFIX_CACHING}" == "true" ]]; then
  command+=(--enable-prefix-caching)
elif [[ "${PREFIX_CACHING}" == "false" ]]; then
  command+=(--no-enable-prefix-caching)
else
  echo "PREFIX_CACHING must be true or false" >&2
  exit 64
fi

if [[ "${TRACING_ENABLED}" == "true" ]]; then
  if [[ -z "${OTLP_TRACES_ENDPOINT:-}" ]]; then
    echo "OTLP_TRACES_ENDPOINT is required when tracing is enabled" >&2
    exit 64
  fi
  command+=(--otlp-traces-endpoint "${OTLP_TRACES_ENDPOINT}")
elif [[ "${TRACING_ENABLED}" != "false" ]]; then
  echo "TRACING_ENABLED must be true or false" >&2
  exit 64
fi

if [[ "${KV_EVENTS_ENABLED}" == "true" ]]; then
  printf -v kv_topic 'kv@%s:%s@%s' "${POD_IP}" "${runtime_port}" "${SERVED_MODEL_NAME}"
  printf -v kv_events_config \
    '{"enable_kv_cache_events":true,"publisher":"zmq","endpoint":"tcp://*:%s","topic":"%s"}' \
    "${KV_EVENTS_PORT}" "${kv_topic}"
  command+=(--kv-events-config "${kv_events_config}")
elif [[ "${KV_EVENTS_ENABLED}" != "false" ]]; then
  echo "KV_EVENTS_ENABLED must be true or false" >&2
  exit 64
fi

exec "${command[@]}"
