resource "terraform_data" "bootstrap" {
  triggers_replace = [
    var.host,
    tostring(var.ssh_port),
    var.k3s_version,
    var.k3s_installer_sha256,
    tostring(var.expected_gpu_count),
    var.gpu_sku,
    var.node_name,
    var.model_cache_root,
    var.engine_cache_root,
    filesha256("${path.module}/../../../scripts/bootstrap-gpu-host.sh"),
  ]

  connection {
    type        = "ssh"
    host        = var.host
    port        = var.ssh_port
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
      "sudo env K3S_VERSION='${var.k3s_version}' K3S_INSTALLER_SHA256='${var.k3s_installer_sha256}' INFERSCALE_GPU_SKU='${var.gpu_sku}' INFERSCALE_EXPECTED_GPU_COUNT='${var.expected_gpu_count}' INFERSCALE_NODE_NAME='${var.node_name}' INFERSCALE_MODEL_CACHE_ROOT='${var.model_cache_root}' INFERSCALE_ENGINE_CACHE_ROOT='${var.engine_cache_root}' /tmp/inferscale-bootstrap-gpu-host.sh",
      "test \"$(nvidia-smi --query-gpu=index --format=csv,noheader | wc -l)\" -eq '${var.expected_gpu_count}'",
      "rm -f /tmp/inferscale-bootstrap-gpu-host.sh",
    ]
  }
}
