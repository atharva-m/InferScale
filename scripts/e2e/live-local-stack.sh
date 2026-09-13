#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${script_dir}/../lib/common.sh"
repo_root="$(repository_root)"
cd "${repo_root}"

cluster_name="${INFERSCALE_K3D_CLUSTER:-inferscale-dev}"
public_base_url="${INFERSCALE_E2E_PUBLIC_BASE_URL:-http://127.0.0.1:8080}"
postgres_port="${INFERSCALE_E2E_POSTGRES_PORT:-15432}"
api_port="${INFERSCALE_E2E_API_PORT:-18080}"
wait_seconds="${INFERSCALE_E2E_WAIT_SECONDS:-420}"
bootstrap="${INFERSCALE_E2E_BOOTSTRAP:-true}"
exercise_controller_restart="${INFERSCALE_E2E_CONTROLLER_RESTART:-true}"
exercise_valkey_outage="${INFERSCALE_E2E_VALKEY_OUTAGE:-true}"

for command in docker k3d kubectl curl python3 go; do
  require_command "${command}"
done

case "${bootstrap}" in true|false) ;; *) die "INFERSCALE_E2E_BOOTSTRAP must be true or false" ;; esac
case "${exercise_controller_restart}" in true|false) ;; *) die "INFERSCALE_E2E_CONTROLLER_RESTART must be true or false" ;; esac
case "${exercise_valkey_outage}" in true|false) ;; *) die "INFERSCALE_E2E_VALKEY_OUTAGE must be true or false" ;; esac
[[ "${postgres_port}" =~ ^[0-9]+$ ]] && ((postgres_port > 0 && postgres_port < 65536)) \
  || die "INFERSCALE_E2E_POSTGRES_PORT must be a valid TCP port"
[[ "${api_port}" =~ ^[0-9]+$ ]] && ((api_port > 0 && api_port < 65536 && api_port != postgres_port)) \
  || die "INFERSCALE_E2E_API_PORT must be a valid TCP port distinct from the PostgreSQL port"
[[ "${wait_seconds}" =~ ^[0-9]+$ ]] && ((wait_seconds >= 60)) \
  || die "INFERSCALE_E2E_WAIT_SECONDS must be an integer of at least 60"

umask 077
work_dir="$(mktemp -d)"
port_forward_pid=""
api_port_forward_pid=""
valkey_scaled_down=false
tenant_namespace=""
deployment_name=""

diagnostics() {
  echo "Live local-stack E2E failed; collecting bounded, secret-free diagnostics." >&2
  kubectl get nodes -o wide >&2 || true
  kubectl get pods -A >&2 || true
  kubectl get gateway,httproute,inferencepool -A >&2 2>/dev/null || true
  if [[ -n "${tenant_namespace}" ]]; then
    kubectl -n "${tenant_namespace}" get inferencedeployment,deployments,services,jobs,httproute,inferencepool,scaledobject >&2 2>/dev/null || true
    if [[ -n "${deployment_name}" ]]; then
      kubectl -n "${tenant_namespace}" get inferencedeployment "${deployment_name}" -o yaml >&2 2>/dev/null || true
    fi
  fi
  for component in inferscale-api inferscale-controller inferscale-admission; do
    kubectl -n inferscale-system logs "deployment/${component}" --all-containers --tail=120 >&2 2>/dev/null || true
  done
}

cleanup() {
  local status=$?
  trap - EXIT
  if ((status != 0)); then
    diagnostics
  fi
  if [[ "${valkey_scaled_down}" == "true" ]]; then
    kubectl -n inferscale-system scale deployment/valkey --replicas=1 >/dev/null 2>&1 || true
    kubectl -n inferscale-system rollout status deployment/valkey --timeout=120s >/dev/null 2>&1 || true
  fi
  if [[ -n "${port_forward_pid}" ]]; then
    kill "${port_forward_pid}" >/dev/null 2>&1 || true
    wait "${port_forward_pid}" >/dev/null 2>&1 || true
  fi
  if [[ -n "${api_port_forward_pid}" ]]; then
    kill "${api_port_forward_pid}" >/dev/null 2>&1 || true
    wait "${api_port_forward_pid}" >/dev/null 2>&1 || true
  fi
  rm -r -- "${work_dir}"
  exit "${status}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

wait_until() {
  local description="$1"
  shift
  local deadline=$((SECONDS + wait_seconds))
  until "$@"; do
    if ((SECONDS >= deadline)); then
      echo "timed out waiting for ${description}" >&2
      return 1
    fi
    sleep 2
  done
}

json_value() {
  local file="$1"
  local key="$2"
  python3 - "${file}" "${key}" <<'PY'
import json
import pathlib
import sys

value = json.loads(pathlib.Path(sys.argv[1]).read_text())
for component in sys.argv[2].split("."):
    value = value[component]
if not isinstance(value, (str, int, float, bool)):
    raise SystemExit(f"JSON path {sys.argv[2]!r} is not scalar")
print(value)
PY
}

if [[ "${bootstrap}" == "true" ]]; then
  "${repo_root}/scripts/dev/up.sh"
else
  cluster_exists "${cluster_name}" || die "k3d cluster ${cluster_name} does not exist"
  kubectl config use-context "k3d-${cluster_name}" >/dev/null
  "${repo_root}/scripts/dev/status.sh"
fi

kubectl config use-context "k3d-${cluster_name}" >/dev/null
kubectl -n inferscale-gateway wait --for=condition=Programmed gateway/inferscale --timeout="${wait_seconds}s"

# Liveness is intentionally public, but readiness contains dependency state and
# stays cluster-internal. Check both boundaries without reopening /readyz on the
# Gateway merely to make the test convenient.
wait_until "the public management API liveness route" curl --fail --silent --show-error --max-time 5 "${public_base_url}/healthz" --output /dev/null

kubectl -n inferscale-system port-forward --address 127.0.0.1 service/inferscale-api "${api_port}:8080" \
  >"${work_dir}/api-port-forward.log" 2>&1 &
api_port_forward_pid=$!

api_port_forward_ready() {
  kill -0 "${api_port_forward_pid}" 2>/dev/null &&
    grep -Fq "Forwarding from 127.0.0.1:${api_port}" "${work_dir}/api-port-forward.log"
}
wait_until "the internal API port-forward" api_port_forward_ready
internal_api_url="http://127.0.0.1:${api_port}"
wait_until "the cluster-internal API readiness endpoint" curl --fail --silent --show-error --max-time 5 "${internal_api_url}/readyz" --output /dev/null

kubectl -n inferscale-system port-forward --address 127.0.0.1 service/postgres "${postgres_port}:5432" \
  >"${work_dir}/postgres-port-forward.log" 2>&1 &
port_forward_pid=$!

port_forward_ready() {
  kill -0 "${port_forward_pid}" 2>/dev/null &&
    grep -Fq "Forwarding from 127.0.0.1:${postgres_port}" "${work_dir}/postgres-port-forward.log"
}
wait_until "the PostgreSQL port-forward" port_forward_ready

go build -trimpath -o "${work_dir}/inferscalectl" ./cmd/inferscalectl
database_url="postgres://inferscale:local-development-only@127.0.0.1:${postgres_port}/inferscale?sslmode=disable"

run_ctl() {
  INFERSCALE_ENV=development \
    INFERSCALE_DATABASE_URL="${database_url}" \
    INFERSCALE_FAKE_RUNTIME=false \
    INFERSCALE_FEATURE_BACKEND_AUTO=false \
    INFERSCALE_FEATURE_TRTLLM=false \
    INFERSCALE_FEATURE_PROGRESSIVE_ROLLOUT=false \
    INFERSCALE_FEATURE_SCALE_TO_ZERO=false \
    "${work_dir}/inferscalectl" "$@"
}

run_suffix="$(date -u +%s)-$$"
tenant_slug="live-e2e-${run_suffix}"
tenant_namespace="tenant-${tenant_slug}"
deployment_name="chat-${run_suffix}"
key_name="live-e2e-${run_suffix}"

run_ctl tenant create \
  --slug "${tenant_slug}" \
  --name "Live local-stack E2E ${run_suffix}" \
  --max-deployments 2 \
  --max-gpus 2 \
  --requests-per-minute 600 \
  --max-concurrent-requests 16 \
  --max-queued-requests 32 \
  >"${work_dir}/tenant.json"
tenant_id="$(json_value "${work_dir}/tenant.json" id)"

run_ctl apikey issue --tenant "${tenant_id}" --name "${key_name}" >"${work_dir}/key.json"
api_key="$(json_value "${work_dir}/key.json" apiKey)"

python3 - "${work_dir}/create.json" "${deployment_name}" <<'PY'
import json
import pathlib
import sys

pathlib.Path(sys.argv[1]).write_text(json.dumps({
    "name": sys.argv[2],
    "model": {
        "uri": "hf://Qwen/Qwen3-0.6B",
        "revision": "b" * 40,
    },
    "backend": "vllm",
    "precision": "bf16",
    "quantization": "none",
    "gpu": {"type": "local-fake", "count": 1},
    "tensor_parallelism": 1,
    "max_model_len": 2048,
    "prefix_caching": False,
    "min_replicas": 1,
    "max_replicas": 2,
    "admission": {
        "maxConcurrentRequests": 4,
        "maxQueuedRequests": 8,
        "priorityClass": "standard",
    },
    "routing": {"policy": "load-aware"},
    "rollout": {"strategy": "progressive", "shadowPercent": 10},
    "observability": {"tracing": True},
}))
PY

create_status="$(curl --silent --show-error --max-time 30 \
  --output "${work_dir}/create-response.json" \
  --write-out '%{http_code}' \
  --request POST "${public_base_url}/v1/deployments" \
  --header "Authorization: Bearer ${api_key}" \
  --header 'Content-Type: application/json' \
  --header "Idempotency-Key: live-e2e-create-${run_suffix}" \
  --data-binary "@${work_dir}/create.json")"
if [[ "${create_status}" != "202" ]]; then
  echo "deployment create returned HTTP ${create_status}: $(<"${work_dir}/create-response.json")" >&2
  exit 1
fi
deployment_id="$(json_value "${work_dir}/create-response.json" deployment.id)"

cr_ready() {
  kubectl -n "${tenant_namespace}" get inferencedeployment "${deployment_name}" -o json \
    >"${work_dir}/deployment-cr.json" 2>/dev/null || return 1
  DEPLOYMENT_ID="${deployment_id}" python3 - "${work_dir}/deployment-cr.json" <<'PY'
import json
import os
import pathlib
import sys

resource = json.loads(pathlib.Path(sys.argv[1]).read_text())
status = resource.get("status", {})
conditions = {item.get("type"): item.get("status") for item in status.get("conditions", [])}
assert resource.get("metadata", {}).get("annotations", {}).get("inferscale.io/deployment-id") == os.environ["DEPLOYMENT_ID"]
assert status.get("phase") == "Ready"
assert status.get("observedGeneration") == resource.get("metadata", {}).get("generation")
assert status.get("cache", {}).get("weights") == "NotRequired"
assert status.get("revision", {}).get("stable")
assert not status.get("revision", {}).get("candidate")
for required in ("RuntimeReady", "RouteReady", "ModelCached"):
    assert conditions.get(required) == "True", (required, conditions)
PY
}
wait_until "the projected CR, fake runtime, InferencePool, and HTTPRoute to become Ready" cr_ready

worker_ready() {
  kubectl -n "${tenant_namespace}" get deployments -l app.kubernetes.io/component=model-server -o json \
    >"${work_dir}/workers.json" 2>/dev/null || return 1
  python3 - "${work_dir}/workers.json" <<'PY'
import json
import pathlib
import sys

items = json.loads(pathlib.Path(sys.argv[1]).read_text()).get("items", [])
assert len(items) == 1, len(items)
worker = items[0]
assert worker.get("status", {}).get("availableReplicas", 0) >= 1
assert worker["spec"]["template"]["spec"]["containers"][0]["image"] == "ghcr.io/inferscale/fake-runtime:0.1.0-dev"
PY
}
wait_until "one available CPU fake-runtime worker" worker_ready

if kubectl -n "${tenant_namespace}" get jobs -l app.kubernetes.io/component=model-prefetch -o name 2>/dev/null | grep -q .; then
  die "the local fake runtime unexpectedly created a model-prefetch Job"
fi

api_projection_ready() {
  local status
  status="$(curl --silent --show-error --max-time 10 \
    --output "${work_dir}/deployment-api.json" \
    --write-out '%{http_code}' \
    "${public_base_url}/v1/deployments/${deployment_id}" \
    --header "Authorization: Bearer ${api_key}")" || return 1
  [[ "${status}" == "200" ]] || return 1
  python3 - "${work_dir}/deployment-api.json" <<'PY'
import json
import pathlib
import sys

deployment = json.loads(pathlib.Path(sys.argv[1]).read_text())
assert deployment.get("status", {}).get("phase") == "Ready"
assert deployment.get("stableRevisionId")
assert not deployment.get("candidateRevisionId")
PY
}
wait_until "controller status projection back into PostgreSQL" api_projection_ready

python3 - "${work_dir}/chat.json" "${deployment_name}" <<'PY'
import json
import pathlib
import sys

pathlib.Path(sys.argv[1]).write_text(json.dumps({
    "model": sys.argv[2],
    "messages": [{"role": "user", "content": "local control-plane smoke"}],
    "stream": False,
    "max_tokens": 8,
}))
PY

chat_request() {
  curl --silent --show-error --max-time 30 \
    --output "$1" \
    --write-out '%{http_code}' \
    --request POST "${public_base_url}/v1/deployments/${deployment_id}/chat/completions" \
    --header "Authorization: Bearer ${api_key}" \
    --header 'Content-Type: application/json' \
    --data-binary "@${work_dir}/chat.json"
}

chat_succeeds() {
  local status
  status="$(chat_request "${work_dir}/chat-response.json")" || return 1
  [[ "${status}" == "200" ]] || return 1
  python3 - "${work_dir}/chat-response.json" "${deployment_name}" <<'PY'
import json
import pathlib
import sys

response = json.loads(pathlib.Path(sys.argv[1]).read_text())
assert response.get("model") == sys.argv[2]
assert response.get("choices", [{}])[0].get("message", {}).get("content") == "InferScale local fake runtime response."
PY
}
wait_until "one authenticated request through Gateway, admission, EPP, and the fake runtime" chat_succeeds

stable_revision="$(json_value "${work_dir}/deployment-cr.json" status.revision.stable)"
if [[ "${exercise_controller_restart}" == "true" ]]; then
  kubectl -n inferscale-system rollout restart deployment/inferscale-controller
  kubectl -n inferscale-system rollout status deployment/inferscale-controller --timeout="${wait_seconds}s"
  wait_until "the deployment to remain Ready after a controller restart" cr_ready
  restarted_revision="$(json_value "${work_dir}/deployment-cr.json" status.revision.stable)"
  [[ "${restarted_revision}" == "${stable_revision}" ]] || die "controller restart changed the immutable stable revision"
  wait_until "inference after the controller restart" chat_succeeds
fi

if [[ "${exercise_valkey_outage}" == "true" ]]; then
  kubectl -n inferscale-system scale deployment/valkey --replicas=0
  valkey_scaled_down=true
  valkey_pods_gone() {
    [[ -z "$(kubectl -n inferscale-system get pods -l app.kubernetes.io/name=valkey -o name 2>/dev/null)" ]]
  }
  wait_until "all Valkey Pods to stop" valkey_pods_gone
  outage_status="$(chat_request "${work_dir}/valkey-outage-response.json")"
  if [[ "${outage_status}" != "503" ]]; then
    echo "inference during the Valkey outage returned HTTP ${outage_status}, want 503" >&2
    exit 1
  fi
  kubectl -n inferscale-system scale deployment/valkey --replicas=1
  valkey_scaled_down=false
  kubectl -n inferscale-system rollout status deployment/valkey --timeout="${wait_seconds}s"
  wait_until "inference recovery after Valkey returns" chat_succeeds
fi

echo "Live local-stack E2E passed."
echo "  tenant:     ${tenant_id} (${tenant_namespace})"
echo "  deployment: ${deployment_id} (${deployment_name})"
echo "  coverage:   real PostgreSQL/Valkey, API outbox, apiserver SSA, controller, Gateway/admission/EPP smoke, CPU fake runtime"
echo "  excluded:   GPU runtimes, performance, and Gateway/llm-d conformance claims"
