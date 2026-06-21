<div align="center">
  <img src="https://github.com/postgres.png" alt="PostgreSQL Logo" width="120"/>
</div>

# Terraform Provider for PostgreSQL Anywhere

[![Tests](https://github.com/pinotelio/terraform-provider-postgresql-anywhere/actions/workflows/test.yml/badge.svg)](https://github.com/pinotelio/terraform-provider-postgresql-anywhere/actions/workflows/test.yml)
[![Lint](https://github.com/pinotelio/terraform-provider-postgresql-anywhere/actions/workflows/golangci.yml/badge.svg)](https://github.com/pinotelio/terraform-provider-postgresql-anywhere/actions/workflows/golangci.yml)
[![Release](https://github.com/pinotelio/terraform-provider-postgresql-anywhere/actions/workflows/release.yml/badge.svg)](https://github.com/pinotelio/terraform-provider-postgresql-anywhere/actions/workflows/release.yml)
[![License: MPL-2.0](https://img.shields.io/badge/License-MPL%202.0-brightgreen.svg)](https://opensource.org/licenses/MPL-2.0)
[![Terraform Registry](https://img.shields.io/badge/terraform-registry-623CE4?logo=terraform)](https://registry.terraform.io/providers/pinotelio/postgresql-anywhere)
[![Go Version](https://img.shields.io/github/go-mod/go-version/pinotelio/terraform-provider-postgresql-anywhere)](https://go.dev)

Manage PostgreSQL objects (databases, roles, grants, schemas, extensions, and
more) with Terraform, against a database anywhere: directly reachable, or sitting
in a private subnet on AWS, Azure, GCP, or on-prem. The provider opens the
network path for you through a pluggable transport, so you do not have to
pre-establish a tunnel yourself.

## Highlights

- **Connect anywhere.** Public, or private behind AWS, Azure, GCP, or on-prem,
  through a built-in transport (SSH bastion, AWS SSM, Azure Bastion, or GCP IAP).
- **Embedded Security** Every transport is pure Go, with no `aws`, `az`,
  `gcloud`, `session-manager-plugin`, or `ssh` binary to install or keep patched.
  The provider runs as-is
  on locked-down managed runners such as Terraform Cloud, Scalr, and Spacelift without needing any pre-requisites
  with no privileged setup steps.
- **Keyless by default.** The AWS, Azure, and GCP transports authenticate through
  each cloud's default credential chain, following the recommended practice of
  short-lived, federated credentials via OIDC / workload-identity.

## Supported resources

| Category | Resources | |
|---|---|:--:|
| Databases & schemas | `database`, `schema`, `role` | ✅ |
| Privileges | `grant`, `grant_role`, `default_privileges` | ✅ |
| Objects | `extension`, `function`, `index`, `security_label` | ✅ |
| Replication | `publication`, `subscription`, `replication_slot`, `physical_replication_slot` | ✅ |
| Foreign data | `server`, `user_mapping` | ✅ |
| Data sources | `schemas`, `tables`, `sequences` | ✅ |

All resources support `terraform import`.

## Connectivity (the "anywhere" part)

Pick at most one transport block. With none, the provider connects directly.

| Transport | Reaches | How |
|---|---|---|
| *(none)* | Any publicly reachable PostgreSQL | Direct connection. |
| `ssh_bastion` | Any private PostgreSQL on AWS, Azure, GCP, or on-prem | Built-in SSH tunnel through a bastion you can reach, no CLI. One hop to the database. |
| `aws_ssm` | Private AWS RDS | Built-in AWS SSM port-forwarding through a bastion EC2 instance, no CLI. Auth via the AWS default credential chain (OIDC-friendly). The bastion needs no public IP and no inbound port. |
| `azure_bastion` | Private Azure PostgreSQL (via a jump VM) | Built-in Azure Bastion tunnel, no CLI. Auth via Azure AD (OIDC-friendly). Experimental, see note below. |
| `gcp_iap` | Private GCP PostgreSQL (via a jump VM) | Built-in IAP TCP forwarding, no CLI. Auth via Google Application Default Credentials (OIDC-friendly). |

Notes:

- `aws_ssm`, `azure_bastion`, and `gcp_iap` are the cloud-native, no-inbound
  options (control-plane mediated). `ssh_bastion` is the universal option but
  needs a bastion you can actually reach.
- `aws_ssm` forwards to `host`:`port` directly: the bastion's SSM agent opens the
  connection onward to the database, so it reaches a managed RDS endpoint in one
  hop with no relay on the bastion. `azure_bastion` and `gcp_iap`, by contrast,
  terminate at the target VM's own port, so reaching a managed database means that
  VM must relay onward to it (set `resource_port` or `instance_port` accordingly).
  For self-managed Postgres on the VM, point them at the database port directly.
- `azure_bastion` is **experimental**: Azure does not publicly document the
  Bastion tunnel data-plane protocol, so this implementation follows the flow used
  by `az network bastion tunnel` and should be validated against a real Azure
  Bastion (Standard/Premium SKU, native client enabled) before you rely on it.
- Transports require `scheme = "postgres"`. TLS terminates at the database (the
  tunnel only forwards TCP), so use `sslmode = "require"` or `"verify-ca"`, not
  `"verify-full"`.

## Authentication

Password, AWS RDS IAM, Azure AD identity, and GCP IAM. The AWS paths use the SDK
default credential chain, so keyless OIDC web-identity (for example a Scalr or
GitHub Actions OIDC role) works with no static keys: leave `profile` unset and
the role is assumed from the environment.

## Requirements

- Terraform >= 1.0
- Go >= 1.26 (only to build from source)

## Using the provider

Declare the provider once:

```hcl
terraform {
  required_providers {
    postgresql = {
      source = "pinotelio/postgresql-anywhere"
    }
  }
}
```

The examples below show only the provider configuration. Pick at most one
transport block.

### Direct (publicly reachable database)

```hcl
provider "postgresql" {
  host     = "db.example.com"
  port     = 5432
  username = "postgres"
  password = var.db_password        # as sensitive variables
  sslmode  = "require"
}
```

### AWS SSM (static credentials)

```hcl
provider "postgresql" {
  host     = "mydb.abc123.eu-west-1.rds.amazonaws.com"
  port     = 5432
  username = "terraform"
  password = var.db_password        # as sensitive variables
  sslmode  = "require"

  aws_ssm {
    region        = "eu-west-1"
    access_key    = var.aws_access_key       # static key, inline
    secret_key    = var.aws_secret_key       # static secret, inline
    # session_token = var.aws_session_token  # for temporary credentials
    instance_name = "bastion_ec2"            # or instance_id / instance_tags
  }
}
```

Alternatively, omit `access_key`/`secret_key` and let the default credential
chain pick the keys up from `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` or a
named `profile`. Inline keys (and OIDC, below) are the recommended options.

### AWS SSM (keyless OIDC with assume role)

```hcl
# Leave `profile` unset so the default credential chain resolves the runner's
# OIDC web-identity (AWS_ROLE_ARN + AWS_WEB_IDENTITY_TOKEN_FILE). `role_arn`
# optionally assumes a further role for the session.
provider "postgresql" {
  host     = "mydb.abc123.eu-west-1.rds.amazonaws.com"
  port     = 5432
  username = "terraform"
  password = var.db_password        # as sensitive variables
  sslmode  = "require"

  aws_ssm {
    region        = "eu-west-1"
    instance_name = "bastion"
  }
}
```

### SSH bastion with key auth (any cloud or on-prem)

```hcl
provider "postgresql" {
  host     = "mydb.internal" # endpoint as seen from the bastion
  port     = 5432
  username = "terraform"
  password = var.db_password        # as sensitive variables
  sslmode  = "require"

  ssh_bastion {
    host        = "203.0.113.10"
    user        = "ec2-user"
    private_key = var.bastion_private_key # PEM contents, as a sensitive variable
    host_key    = "ssh-ed25519 AAAA..."   # bastion host key (authorized_keys format)
  }
}
```

`private_key` takes the PEM key contents, not a path. On managed runners pass it
as a sensitive variable;

### SSH bastion with password auth (any cloud or on-prem)

```hcl
provider "postgresql" {
  host     = "mydb.internal"
  port     = 5432
  username = "terraform"
  password = var.db_password        # as sensitive variables
  sslmode  = "require"

  ssh_bastion {
    host     = "203.0.113.10"
    user     = "deploy"
    password = var.bastion_password   # SSH bastion password, a sensitive variable
    host_key = "ssh-ed25519 AAAA..."  # bastion host key (authorized_keys format)
  }
}
```


### GCP IAP

```hcl
# Credentials come from Google Application Default Credentials
# (workload-identity / OIDC, or GOOGLE_APPLICATION_CREDENTIALS).
provider "postgresql" {
  username = "terraform"
  password = var.db_password        # as sensitive variables
  sslmode  = "require"

  gcp_iap {
    project       = "my-project"
    zone          = "europe-west1-b"
    instance      = "jump-vm"
    instance_port = 5432              # the port on the instance that reaches PostgreSQL
  }
}
```

### Azure Bastion (experimental)

```hcl
# Credentials come from DefaultAzureCredential (managed / workload identity,
# or AZURE_CLIENT_ID / AZURE_CLIENT_SECRET / AZURE_TENANT_ID).
provider "postgresql" {
  username = "terraform"
  password = var.db_password        # as sensitive variables
  sslmode  = "require"

  azure_bastion {
    bastion_name       = "my-bastion"
    resource_group     = "my-rg"
    target_resource_id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/my-rg/providers/Microsoft.Compute/virtualMachines/jump-vm"
    resource_port      = 5432     # the port on the VM that reaches PostgreSQL
  }
}
```

See [`examples/`](examples) for complete, runnable configurations.

## Documentation

Reference documentation for the provider, its resources, and data sources is
published on the [Terraform Registry](https://registry.terraform.io/providers/pinotelio/postgresql-anywhere/latest/docs).

## Development

`make build` to build, `make test` for unit tests (no network). Acceptance tests
(`make testacc`) run against a real PostgreSQL instance. See
[CONTRIBUTING.md](CONTRIBUTING.md) for the full setup.

## License

Distributed under the terms of the [LICENSE](LICENSE) file ([MPL-2.0](https://opensource.org/licenses/MPL-2.0)).
