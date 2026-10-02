# GPU host bootstrap

InferScale v1 uses a pre-existing single physical Ubuntu GPU host. On Vast.ai,
rent a full VM with systemd/sudo and GPU passthrough, not an ordinary Docker
instance. See the [4/8-GPU walkthrough](../../docs/deployment/vast-5090.md).
Provider account and instance procurement stay outside this repository;
Terraform connects to the explicitly supplied host and runs the bootstrap script.

The bootstrap deliberately does not install NVIDIA drivers. Validate the provider image with `nvidia-smi` first, set an explicit `K3S_VERSION`, and run:

```bash
sudo K3S_VERSION=<verified-version> \
  K3S_INSTALLER_SHA256=<verified-get-k3s-installer-sha256> \
  INFERSCALE_EXPECTED_GPU_COUNT=8 \
  INFERSCALE_NODE_NAME=inferscale-gpu-01 \
  INFERSCALE_GPU_SKU=RTX_5090 \
  scripts/bootstrap-gpu-host.sh
NVIDIA_DEVICE_PLUGIN_CHART_VERSION=<verified-version> \
DCGM_EXPORTER_CHART_VERSION=<verified-version> \
scripts/install-gpu-components.sh
scripts/verify-remote-gpu.sh 8 --node inferscale-gpu-01
```

All 2/4-GPU TP experiments must pass the single-node invariant check.

The host must already have `nvidia-container-runtime`; bootstrap sets k3s's
default runtime to NVIDIA and labels only the selected node. Existing k3s
installations with another runtime fail with a maintenance instruction instead
of being silently restarted/reconfigured. Set `INFERSCALE_K3S_TLS_SAN` only when
you intentionally need a different verified API hostname/IP.

GPU components live in privileged `nvidia-device-plugin` and
`inferscale-gpu-system` namespaces. DCGM port 9400 is reachable from monitoring;
`inferscale-monitoring` remains restricted. Chart values disable MIG/sharing,
select labeled GPU nodes without requiring NFD, and use the NVIDIA runtime.

Verification checks the selected node, Ready status, physical product/count,
exclusive Kubernetes capacity/allocatable count, and its hostname label. Run it
on the actual GPU VM, using that VM's kubeconfig. Eight is physical capacity,
not TP8. GPU arithmetic and serving/collective tests must still run afterward.
