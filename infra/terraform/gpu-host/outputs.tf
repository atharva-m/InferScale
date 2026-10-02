output "host" {
  value = var.host
}

output "kubeconfig_command" {
  value = "ssh -p ${var.ssh_port} ${var.ssh_user}@${var.host} sudo cat /etc/rancher/k3s/k3s.yaml"
}

output "expected_gpu_count" {
  value = var.expected_gpu_count
}

output "gpu_sku" {
  value = var.gpu_sku
}

output "node_name" {
  value = var.node_name
}
