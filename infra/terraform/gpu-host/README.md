# Existing GPU host bootstrap

This module does not purchase cloud capacity. It connects to one explicitly identified, already-provisioned Ubuntu host, copies the audited bootstrap script, installs the requested immutable k3s version, prepares node-local cache directories, and verifies the expected physical GPU count.

```bash
cp terraform.tfvars.example terraform.tfvars
terraform init
terraform plan
terraform apply
```

The private key content is read at apply time and is not assigned to a Terraform resource attribute, but operators must still secure local Terraform state. Install the NVIDIA device plugin and DCGM exporter with the separate version-explicit script after bootstrap. Never use multiple host addresses for a TP experiment.

For Vast.ai, use a full VM template and copy `host`, `ssh_user`, and `ssh_port`
from its SSH connection command. `expected_gpu_count` accepts 1/2/4/8 physical
GPUs; TP per worker remains 1/2/4. `node_name` defaults to `inferscale-gpu-01`.
Use that node's actual hostname label when rendering the release. The
[full deployment guide](../../../docs/deployment/vast-5090.md) also provides a
manual path that does not require Terraform.
