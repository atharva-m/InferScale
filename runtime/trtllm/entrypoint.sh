#!/usr/bin/env bash
set -euo pipefail

required=(ENGINE_PATH TOKENIZER_PATH TP_SIZE SERVED_MODEL_NAME MODEL_REVISION PORT PRECISION QUANTIZATION MAX_MODEL_LEN RUNTIME_VERSION RUNTIME_IMAGE_DIGEST GPU_ARCHITECTURE)
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

if [[ ! -d "${ENGINE_PATH}" ]]; then
  echo "TensorRT-LLM engine directory does not exist: ${ENGINE_PATH}" >&2
  exit 66
fi
if [[ ! -e "${TOKENIZER_PATH}" ]]; then
  echo "TensorRT-LLM tokenizer path does not exist: ${TOKENIZER_PATH}" >&2
  exit 66
fi

engine_verifier="${ENGINE_VERIFIER:-/opt/inferscale/build-engine}"
if [[ ! -f "${engine_verifier}" || ! -r "${engine_verifier}" ]]; then
  echo "TensorRT-LLM engine verifier is unavailable: ${engine_verifier}" >&2
  exit 69
fi
bash "${engine_verifier}" --verify-only \
  --model "${TOKENIZER_PATH}" \
  --output "${ENGINE_PATH}" \
  --precision "${PRECISION}" \
  --quantization "${QUANTIZATION}" \
  --tensor-parallelism "${TP_SIZE}" \
  --max-model-len "${MAX_MODEL_LEN}" \
  --model-revision "${MODEL_REVISION}" \
  --runtime-version "${RUNTIME_VERSION}" \
  --runtime-image-digest "${RUNTIME_IMAGE_DIGEST}" \
  --gpu-architecture "${GPU_ARCHITECTURE}"
if [[ ! "${TP_SIZE}" =~ ^[1-9][0-9]*$ ]]; then
  echo "TP_SIZE must be a positive integer" >&2
  exit 64
fi
if [[ ! "${PORT}" =~ ^[0-9]+$ ]] || (( PORT < 1 || PORT > 65535 )); then
  echo "PORT must be in the range 1-65535" >&2
  exit 64
fi

command=(
  trtllm-serve "${ENGINE_PATH}"
  --backend trt
  --tokenizer "${TOKENIZER_PATH}"
  --host 0.0.0.0
  --port "${PORT}"
  --tp_size "${TP_SIZE}"
)

exec "${command[@]}"
