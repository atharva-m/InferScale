# GPU host bootstrap

InferScale v1 uses a pre-existing single physical Ubuntu GPU host. Provider account and instance procurement stay outside this repository; Terraform connects to the explicitly supplied host and runs the checked-in bootstrap script.

The bootstrap deliberately does not install NVIDIA drivers. Validate the provider image with `nvidia-smi` first, set an explicit `K3S_VERSION`, and run:

```bash
sudo K3S_VERSION=<verified-version> \
  K3S_INSTALLER_SHA256=<verified-get-k3s-installer-sha256> \
  INFERSCALE_GPU_SKU=RTX_5090 \
  scripts/bootstrap-gpu-host.sh
NVIDIA_DEVICE_PLUGIN_CHART_VERSION=<verified-version> \
DCGM_EXPORTER_CHART_VERSION=<verified-version> \
scripts/install-gpu-components.sh
scripts/verify-remote-gpu.sh 4
```

All 2/4-GPU TP experiments must pass the single-node invariant check.
