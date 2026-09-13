variable "host" {
  description = "IPv4 address or DNS name of the already-provisioned Ubuntu GPU host."
  type        = string
  validation {
    condition     = length(trimspace(var.host)) > 0
    error_message = "host is required."
  }
}

variable "ssh_user" {
  description = "Unprivileged SSH user with passwordless sudo."
  type        = string
  default     = "root"
  validation {
    condition     = can(regex("^[A-Za-z_][A-Za-z0-9_-]*$", var.ssh_user))
    error_message = "ssh_user must be a simple operating-system user name."
  }
}

variable "ssh_private_key_path" {
  description = "Local path to the SSH private key; key content is not stored in Terraform state."
  type        = string
  validation {
    condition     = length(trimspace(var.ssh_private_key_path)) > 0
    error_message = "ssh_private_key_path is required."
  }
}

variable "k3s_version" {
  description = "Explicit verified k3s release, for example v1.36.2+k3s1."
  type        = string
  validation {
    condition     = can(regex("^v[0-9]+\\.[0-9]+\\.[0-9]+\\+k3s[0-9]+$", var.k3s_version))
    error_message = "k3s_version must be an immutable k3s release."
  }
}

variable "k3s_installer_sha256" {
  description = "Verified sha256 of https://get.k3s.io captured for this bootstrap run."
  type        = string
  validation {
    condition     = can(regex("^[0-9a-f]{64}$", var.k3s_installer_sha256))
    error_message = "k3s_installer_sha256 must be a lowercase 64-hex digest."
  }
}

variable "expected_gpu_count" {
  description = "Expected co-located RTX 5090 count."
  type        = number
  validation {
    condition     = contains([1, 2, 4], var.expected_gpu_count)
    error_message = "expected_gpu_count must be 1, 2, or 4."
  }
}

variable "gpu_sku" {
  description = "Canonical InferScale GPU SKU written to the Kubernetes node label."
  type        = string
  default     = "RTX_5090"
  validation {
    condition     = can(regex("^[A-Za-z0-9]([A-Za-z0-9_.-]*[A-Za-z0-9])?$", var.gpu_sku))
    error_message = "gpu_sku must be a Kubernetes label value."
  }
}

variable "model_cache_root" {
  type    = string
  default = "/var/lib/inferscale/models"
  validation {
    condition     = can(regex("^/[A-Za-z0-9._/-]+$", var.model_cache_root)) && var.model_cache_root != "/" && !strcontains(var.model_cache_root, "..")
    error_message = "model_cache_root must be a non-root absolute path without shell metacharacters or '..'."
  }
}

variable "engine_cache_root" {
  type    = string
  default = "/var/lib/inferscale/engines"
  validation {
    condition     = can(regex("^/[A-Za-z0-9._/-]+$", var.engine_cache_root)) && var.engine_cache_root != "/" && !strcontains(var.engine_cache_root, "..")
    error_message = "engine_cache_root must be a non-root absolute path without shell metacharacters or '..'."
  }
}
