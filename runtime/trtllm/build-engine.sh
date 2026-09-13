#!/usr/bin/env bash
set -euo pipefail

model=""
output=""
precision=""
quantization="none"
tensor_parallelism=""
max_model_len=""
model_revision=""
runtime_version=""
runtime_image_digest=""
gpu_architecture=""
verify_only="false"

while (($#)); do
  case "$1" in
    --model) model="${2:-}"; shift 2 ;;
    --output) output="${2:-}"; shift 2 ;;
    --precision) precision="${2:-}"; shift 2 ;;
    --quantization) quantization="${2:-}"; shift 2 ;;
    --tensor-parallelism) tensor_parallelism="${2:-}"; shift 2 ;;
    --max-model-len) max_model_len="${2:-}"; shift 2 ;;
    --model-revision) model_revision="${2:-}"; shift 2 ;;
    --runtime-version) runtime_version="${2:-}"; shift 2 ;;
    --runtime-image-digest) runtime_image_digest="${2:-}"; shift 2 ;;
    --gpu-architecture) gpu_architecture="${2:-}"; shift 2 ;;
    --verify-only) verify_only="true"; shift ;;
    *) echo "unsupported engine-build argument: $1" >&2; exit 64 ;;
  esac
done

for value in model output precision tensor_parallelism max_model_len model_revision runtime_version runtime_image_digest gpu_architecture; do
  if [[ -z "${!value}" ]]; then
    echo "missing required engine-build value: ${value}" >&2
    exit 64
  fi
done
model_revision="${model_revision,,}"
if [[ ! "${model_revision}" =~ ^[0-9a-f]{40}$ ]]; then
  echo "model revision must be an immutable lowercase 40-character commit SHA" >&2
  exit 64
fi
if [[ ! "${runtime_image_digest}" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  echo "runtime image digest must be sha256:<64 lowercase hex>" >&2
  exit 64
fi
if [[ "${precision}" != "bf16" || "${quantization}" != "none" ]]; then
  echo "the pinned TensorRT engine builder supports only bf16 with quantization=none" >&2
  exit 64
fi
if [[ ! "${tensor_parallelism}" =~ ^(1|2|4)$ ]] || [[ ! "${max_model_len}" =~ ^[1-9][0-9]*$ ]]; then
  echo "invalid tensor parallelism or maximum model length" >&2
  exit 64
fi
if [[ ! -d "${model}" ]]; then
  echo "model directory does not exist: ${model}" >&2
  exit 66
fi
if [[ "${output}" != /* || "${output}" == "/" ]]; then
  echo "engine output must be a non-root absolute path" >&2
  exit 64
fi

output_parent="$(dirname -- "${output}")"
output_name="$(basename -- "${output}")"
if [[ -z "${output_name}" || "${output_name}" == "." || "${output_name}" == ".." ]]; then
  echo "engine output must identify one cache entry" >&2
  exit 64
fi
mkdir -p -- "${output_parent}"

verify_engine() {
  local target="$1"
  [[ -d "${target}" && ! -L "${target}" ]] || return 1
  python3 - "${target}" "${model_revision}" "${runtime_version}" "${runtime_image_digest}" \
    "${precision}" "${quantization}" "${tensor_parallelism}" "${max_model_len}" "${gpu_architecture}" <<'PY'
import hashlib
import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
expected = {
    "model_revision": sys.argv[2],
    "runtime_version": sys.argv[3],
    "runtime_image_digest": sys.argv[4],
    "precision": sys.argv[5],
    "quantization": sys.argv[6],
    "tensor_parallelism": int(sys.argv[7]),
    "max_model_len": int(sys.argv[8]),
    "gpu_architecture": sys.argv[9],
}
try:
    manifest_bytes = (root / "manifest.json").read_bytes()
    marker = (root / ".complete").read_text(encoding="ascii").strip()
    manifest = json.loads(manifest_bytes)
    metadata = json.loads((root / "build-metadata.json").read_bytes())
except (OSError, UnicodeError, json.JSONDecodeError):
    raise SystemExit(1)
if marker != hashlib.sha256(manifest_bytes).hexdigest():
    raise SystemExit(1)
if manifest.get("schema_version") != 1 or metadata.get("schema_version") != 1:
    raise SystemExit(1)
for key, value in expected.items():
    if manifest.get(key) != value or metadata.get(key) != value:
        raise SystemExit(1)
if not isinstance(metadata.get("build_seconds"), int) or metadata["build_seconds"] < 0:
    raise SystemExit(1)
files = manifest.get("files")
if not isinstance(files, list) or not files:
    raise SystemExit(1)
declared = set()
if any(path.is_symlink() for path in root.rglob("*")):
    raise SystemExit(1)
for entry in files:
    if not isinstance(entry, dict):
        raise SystemExit(1)
    relative = entry.get("path")
    if not isinstance(relative, str) or not relative or relative.startswith("/"):
        raise SystemExit(1)
    parts = pathlib.PurePosixPath(relative).parts
    if ".." in parts or "." in parts:
        raise SystemExit(1)
    candidate = root.joinpath(*parts)
    if candidate.is_symlink() or not candidate.is_file():
        raise SystemExit(1)
    digest = hashlib.sha256()
    size = 0
    with candidate.open("rb") as handle:
        while chunk := handle.read(1024 * 1024):
            size += len(chunk)
            digest.update(chunk)
    if entry.get("sha256") != digest.hexdigest() or entry.get("size") != size:
        raise SystemExit(1)
    declared.add(relative)
actual = {
    path.relative_to(root).as_posix()
    for path in root.rglob("*")
    if path.is_file() and not path.is_symlink()
    and path.name not in {"manifest.json", ".complete"}
}
if actual != declared or "config.json" not in declared or "build-metadata.json" not in declared:
    raise SystemExit(1)
PY
}

if verify_engine "${output}"; then
  exit 0
fi
if [[ "${verify_only}" == "true" ]]; then
  echo "TensorRT engine cache failed full manifest verification: ${output}" >&2
  exit 66
fi

lock_dir="${output_parent}/.${output_name}.build-lock"
lock_timeout="${ENGINE_BUILD_LOCK_TIMEOUT_SECONDS:-21600}"
lock_stale="${ENGINE_BUILD_LOCK_STALE_SECONDS:-25200}"
if [[ ! "${lock_timeout}" =~ ^[1-9][0-9]*$ || ! "${lock_stale}" =~ ^[1-9][0-9]*$ ]]; then
  echo "engine build lock timeouts must be positive integer seconds" >&2
  exit 64
fi
deadline="$(( $(date +%s) + lock_timeout ))"
while ! mkdir -- "${lock_dir}" 2>/dev/null; do
  if verify_engine "${output}"; then
    exit 0
  fi
  now="$(date +%s)"
  modified="$(stat -c %Y "${lock_dir}" 2>/dev/null || printf '0')"
  if (( modified > 0 && now - modified > lock_stale )); then
    rmdir -- "${lock_dir}" 2>/dev/null || true
    continue
  fi
  if (( now >= deadline )); then
    echo "timed out waiting for TensorRT engine cache lock" >&2
    exit 75
  fi
  sleep 2
done

staging=""
work=""
cleanup() {
  if [[ -n "${staging}" && -d "${staging}" ]]; then
    find "${staging}" -depth -delete 2>/dev/null || true
  fi
  if [[ -n "${work}" && -d "${work}" ]]; then
    find "${work}" -depth -delete 2>/dev/null || true
  fi
  rmdir -- "${lock_dir}" 2>/dev/null || true
}
trap cleanup EXIT

if verify_engine "${output}"; then
  exit 0
fi
if [[ -e "${output}" ]]; then
  quarantine="${output}.corrupt.$(date +%s).$$"
  while [[ -e "${quarantine}" ]]; do
    quarantine="${quarantine}.x"
  done
  mv -- "${output}" "${quarantine}"
  echo "quarantined invalid TensorRT engine cache entry at ${quarantine}" >&2
fi

staging="$(mktemp -d "${output_parent}/.${output_name}.publish.XXXXXX")"
work="$(mktemp -d "${output_parent}/.${output_name}.work.XXXXXX")"
checkpoint="${work}/checkpoint"
converter="${TRTLLM_QWEN_CONVERTER:-/app/tensorrt_llm/examples/models/core/qwen/convert_checkpoint.py}"
if [[ ! -f "${converter}" ]]; then
  echo "pinned Qwen checkpoint converter is missing: ${converter}" >&2
  exit 69
fi

started="$(date +%s)"
python3 "${converter}" \
  --model_dir "${model}" \
  --output_dir "${checkpoint}" \
  --dtype bfloat16 \
  --tp_size "${tensor_parallelism}"
trtllm-build \
  --checkpoint_dir "${checkpoint}" \
  --output_dir "${staging}" \
  --max_seq_len "${max_model_len}" \
  --gpt_attention_plugin bfloat16 \
  --gemm_plugin bfloat16 \
  --workers "${tensor_parallelism}"

if [[ ! -f "${staging}/config.json" ]]; then
  echo "TensorRT engine build did not produce config.json" >&2
  exit 70
fi
duration="$(( $(date +%s) - started ))"
python3 - "${staging}" "${model_revision}" "${runtime_version}" "${runtime_image_digest}" \
  "${precision}" "${quantization}" "${tensor_parallelism}" "${max_model_len}" \
  "${gpu_architecture}" "${duration}" <<'PY'
import hashlib
import json
import os
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
identity = {
    "schema_version": 1,
    "model_revision": sys.argv[2],
    "runtime_version": sys.argv[3],
    "runtime_image_digest": sys.argv[4],
    "precision": sys.argv[5],
    "quantization": sys.argv[6],
    "tensor_parallelism": int(sys.argv[7]),
    "max_model_len": int(sys.argv[8]),
    "gpu_architecture": sys.argv[9],
}
metadata = {**identity, "build_seconds": int(sys.argv[10])}
metadata_path = root / "build-metadata.json"
with metadata_path.open("w", encoding="utf-8") as handle:
    json.dump(metadata, handle, sort_keys=True, separators=(",", ":"))
    handle.write("\n")
    handle.flush()
    os.fsync(handle.fileno())
files = []
for path in sorted(root.rglob("*")):
    if path.is_symlink():
        raise SystemExit(f"engine output contains a symlink: {path}")
    if not path.is_file() or path.name in {"manifest.json", ".complete"}:
        continue
    digest = hashlib.sha256()
    size = 0
    with path.open("rb") as handle:
        while chunk := handle.read(1024 * 1024):
            size += len(chunk)
            digest.update(chunk)
    files.append({
        "path": path.relative_to(root).as_posix(),
        "sha256": digest.hexdigest(),
        "size": size,
    })
manifest = {**identity, "files": files}
manifest_path = root / "manifest.json"
with manifest_path.open("w", encoding="utf-8") as handle:
    json.dump(manifest, handle, sort_keys=True, separators=(",", ":"))
    handle.write("\n")
    handle.flush()
    os.fsync(handle.fileno())
manifest_digest = hashlib.sha256(manifest_path.read_bytes()).hexdigest()
with (root / ".complete").open("w", encoding="ascii") as handle:
    handle.write(manifest_digest + "\n")
    handle.flush()
    os.fsync(handle.fileno())
directory_fd = os.open(root, os.O_RDONLY)
try:
    os.fsync(directory_fd)
finally:
    os.close(directory_fd)
PY

if ! verify_engine "${staging}"; then
  echo "new TensorRT engine failed full manifest verification" >&2
  exit 70
fi
mv -- "${staging}" "${output}"
staging=""
python3 - "${output_parent}" <<'PY'
import os
import sys

descriptor = os.open(sys.argv[1], os.O_RDONLY)
try:
    os.fsync(descriptor)
finally:
    os.close(descriptor)
PY
