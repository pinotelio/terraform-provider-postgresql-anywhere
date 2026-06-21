# Example: manage a private PostgreSQL on GCP through IAP TCP forwarding.
#
# The provider tunnels to a GCE instance's port through GCP Identity-Aware Proxy,
# in-process (no gcloud CLI). The OAuth token comes from Application Default
# Credentials, so workload-identity federation / OIDC works with no static keys.
#
# IAP terminates at the instance's port, so instance_port must reach PostgreSQL:
# point it at the database if it runs on the instance, or at a relay on the
# instance that forwards to a managed Cloud SQL endpoint. (For Cloud SQL itself,
# scheme = "gcppostgres" with the built-in Cloud SQL connector is usually simpler.)
#
# The IAM identity needs the IAP-secured Tunnel User role on the instance, and an
# ingress firewall rule allowing the IAP range (35.235.240.0/20) to the port.

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

  # host/port are not used for routing here: the tunnel target is the instance's
  # instance_port below, reached over a local loopback listener.

  gcp_iap {
    project       = "my-project"
    zone          = "europe-west1-b"
    instance      = "jump-vm"
    instance_port = 5432
    # local_port  = 0 # 0 = OS-chosen free loopback port
  }
}

variable "db_password" {
  type      = string
  sensitive = true
}

resource "postgresql_database" "app" {
  name = "app"
}
