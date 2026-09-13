resource "terraform_data" "bootstrap" {
  triggers_replace = [
    var.host,
    var.k3s_version,
    var.k3s_installer_sha256,
    tostring(var.expected_gpu_count),
    var.gpu_sku,
    filesha256("${path.module}/../../../scripts/bootstrap-gpu-host.sh"),
  ]

  connection {
    type        = "ssh"
    host        = var.host
    user        = var.ssh_user
    private_key = file(pathexpand(var.ssh_private_key_path))
    timeout     = "10m"
  }

  provisioner "file" {
    source      = "${path.module}/../../../scripts/bootstrap-gpu-host.sh"
    destination = "/tmp/inferscale-bootstrap-gpu-host.sh"
  }

  provisioner "remote-exec" {
    inline = [
      "chmod 0500 /tmp/inferscale-bootstrap-gpu-host.sh",
      "sudo env K3S_VERSION='${var.k3s_version}' K3S_INSTALLER_SHA256='${var.k3s_installer_sha256}' INFERSCALE_GPU_SKU='${var.gpu_sku}' INFERSCALE_MODEL_CACHE_ROOT='${var.model_cache_root}' INFERSCALE_ENGINE_CACHE_ROOT='${var.engine_cache_root}' /tmp/inferscale-bootstrap-gpu-host.sh",
      "test \"$(nvidia-smi --query-gpu=index --format=csv,noheader | wc -l)\" -eq '${var.expected_gpu_count}'",
      "rm -f /tmp/inferscale-bootstrap-gpu-host.sh",
    ]
  }
}
