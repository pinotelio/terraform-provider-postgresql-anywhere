# Example: manage a private PostgreSQL through an SSH bastion.
#
# Works for any cloud (AWS / Azure / GCP) or on-prem: the provider opens an SSH
# connection to a reachable bastion host and forwards the database endpoint over
# a local loopback listener. The bastion must be reachable from where Terraform
# runs (public IP, VPN, etc.) and must have network line-of-sight to the
# database.

terraform {
  required_providers {
    postgresql = {
      source = "pinotelio/postgresql-anywhere"
    }
  }
}

provider "postgresql" {
  scheme = "postgres"

  # host/port are the database endpoint as resolvable FROM the bastion.
  host = "mydb.internal.example.com"
  port = 5432

  username = "terraform"
  password = var.db_password
  sslmode  = "require"

  ssh_bastion {
    host = "203.0.113.10"
    port = 22
    user = "ec2-user"

    # Authentication: set one of private_key or password.
    # private_key takes the PEM contents, not a path. On managed runners pass it
    # as a sensitive variable; file("~/.ssh/...") only works for local runs.
    private_key = var.bastion_private_key
    # password  = var.bastion_password

    # Host-key verification: set one of host_key / known_hosts_file, or
    # insecure_ignore_host_key (not recommended).
    host_key = "ssh-ed25519 AAAA..." # bastion public key, authorized_keys format
    # known_hosts_file         = "/path/to/known_hosts"
    # insecure_ignore_host_key = true

    # local_port = 0 # 0 = OS-chosen free loopback port
  }
}

variable "db_password" {
  type      = string
  sensitive = true
}

variable "bastion_private_key" {
  type      = string
  sensitive = true
  # PEM contents of the SSH private key, e.g. set as a sensitive workspace
  # variable on Terraform Cloud / Scalr / Spacelift.
}

resource "postgresql_database" "app" {
  name = "app"
}
