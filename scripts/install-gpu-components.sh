#!/usr/bin/env bash
set -euo pipefail

device_plugin_chart_version="${NVIDIA_DEVICE_PLUGIN_CHART_VERSION:-}"
dcgm_chart_version="${DCGM_EXPORTER_CHART_VERSION:-}"
[[ -n "${device_plugin_chart_version}" ]] || { echo "NVIDIA_DEVICE_PLUGIN_CHART_VERSION is required" >&2; exit 64; }
[[ -n "${dcgm_chart_version}" ]] || { echo "DCGM_EXPORTER_CHART_VERSION is required" >&2; exit 64; }
command -v helm >/dev/null 2>&1 || { echo "helm is required" >&2; exit 1; }
command -v kubectl >/dev/null 2>&1 || { echo "kubectl is required" >&2; exit 1; }

helm repo add nvdp https://nvidia.github.io/k8s-device-plugin --force-update
helm repo add nvidia https://nvidia.github.io/dcgm-exporter/helm-charts --force-update
helm repo update

helm upgrade --install nvidia-device-plugin nvdp/nvidia-device-plugin \
  --namespace nvidia-device-plugin --create-namespace \
  --version "${device_plugin_chart_version}" \
  --set failOnInitError=true \
  --wait --timeout 10m

helm upgrade --install dcgm-exporter nvidia/dcgm-exporter \
  --namespace inferscale-monitoring --create-namespace \
  --version "${dcgm_chart_version}" \
  --set serviceMonitor.enabled=false \
  --set-string service.annotations."prometheus\.io/scrape"=true \
  --set-string service.annotations."prometheus\.io/port"=9400 \
  --wait --timeout 10m

kubectl rollout status -n nvidia-device-plugin daemonset/nvidia-device-plugin --timeout=10m
kubectl rollout status -n inferscale-monitoring daemonset/dcgm-exporter --timeout=10m
