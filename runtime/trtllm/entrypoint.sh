#!/usr/bin/env bash
set -euo pipefail

required=(ENGINE_PATH TOKENIZER_PATH TP_SIZE SERVED_MODEL_NAME MODEL_REVISION PORT PRECISION QUANTIZATION MAX_MODEL_LEN PREFIX_CACHING RUNTIME_VERSION RUNTIME_IMAGE_DIGEST GPU_ARCHITECTURE)
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
if [[ ! "${SERVED_MODEL_NAME}" =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]*$ ]] || (( ${#SERVED_MODEL_NAME} > 253 )); then
  echo "SERVED_MODEL_NAME must be a safe model-name path component" >&2
  exit 64
fi
if [[ "${PREFIX_CACHING}" != true && "${PREFIX_CACHING}" != false ]]; then
  echo "PREFIX_CACHING must be true or false" >&2
  exit 64
fi
if [[ ! "${MAX_MODEL_LEN}" =~ ^[1-9][0-9]*$ ]]; then
  echo "MAX_MODEL_LEN must be a positive integer" >&2
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

# The pinned 1.0.0 OpenAIServer derives its public model identity from the
# engine directory basename; its CLI has no served-model-name option. A private
# symlink supplies the deployment name without changing the verified cache.
serving_root="$(mktemp -d "${TMPDIR:-/tmp}/inferscale-trtllm.XXXXXXXX")"
mkdir "${serving_root}/models"
engine_path="$(cd -- "${ENGINE_PATH}" && pwd -P)"
served_engine_path="${serving_root}/models/${SERVED_MODEL_NAME}"
ln -s -- "${engine_path}" "${served_engine_path}"
# KvCacheConfig defaults block reuse to true. Explicitly apply either requested
# value through the pinned CLI's supported nested LLM options.
options_path="${serving_root}/options.yaml"
printf 'kv_cache_config:\n  enable_block_reuse: %s\n' "${PREFIX_CACHING}" >"${options_path}"

command=(
  trtllm-serve "${served_engine_path}"
  --backend trt
  --tokenizer "${TOKENIZER_PATH}"
  --host 0.0.0.0
  --port "${PORT}"
  --tp_size "${TP_SIZE}"
  --max_seq_len "${MAX_MODEL_LEN}"
  --extra_llm_api_options "${options_path}"
)

exec "${command[@]}"
