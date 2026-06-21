# Example: manage a private PostgreSQL on Azure through Azure Bastion.
#
# The provider tunnels to a VM's port through Azure Bastion, in-process (no az
# CLI). The AAD token comes from DefaultAzureCredential, so workload-identity /
# managed-identity / OIDC works with no static keys.
#
# EXPERIMENTAL: Azure does not publicly document the Bastion tunnel data-plane
# protocol. This follows the flow used by `az network bastion tunnel` and should
# be validated against a real Azure Bastion (Standard/Premium SKU with native
# client support enabled) before you rely on it.
#
# The tunnel terminates at the target VM's resource_port, so that port must reach
# PostgreSQL: the database if it runs on the VM, or a relay to a managed Flexible
# Server endpoint.

terraform {
  required_providers {
    postgresql = {
      source = "pinotelio/postgresql-anywhere"
    }
  }
}

provider "postgresql" {
  scheme   = "postgres" # tunnels are only supported with the "postgres" scheme
  username = "terraform"
  password = var.db_password
  sslmode  = "require"

  # host/port are not used for routing here: the tunnel target is the VM's
  # resource_port below, reached over a local loopback listener.

  azure_bastion {
    bastion_name       = "my-bastion"
    resource_group     = "my-rg"
    target_resource_id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/my-rg/providers/Microsoft.Compute/virtualMachines/jump-vm"
    resource_port      = 5432
    # subscription     = "00000000-0000-0000-0000-000000000000" # optional; parsed from target_resource_id if omitted
    # local_port       = 0 # 0 = OS-chosen free loopback port
  }
}

variable "db_password" {
  type      = string
  sensitive = true
}

resource "postgresql_database" "app" {
  name = "app"
}
