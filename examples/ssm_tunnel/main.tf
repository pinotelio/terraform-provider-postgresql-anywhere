# Example: manage a private AWS RDS PostgreSQL through an in-provider SSM tunnel.
#
# The provider opens an AWS SSM "port forwarding to remote host" session through
# a bastion EC2 instance and connects to the RDS endpoint over a local loopback
# listener. No session-manager-plugin binary is required on the runner, and no
# external `aws ssm start-session` process needs to be kept alive. The bastion
# needs no public IP and no inbound port; the SSM agent dials out to AWS.
#
# Requirements:
#   - A running bastion EC2 instance in the VPC with the SSM agent + an instance
#     profile granting AmazonSSMManagedInstanceCore, and egress to RDS:5432.
#   - The RDS security group must allow ingress on 5432 from the bastion's SG.
#   - The provider's AWS identity needs: ssm:StartSession (on the bastion and the
#     AWS-StartPortForwardingSessionToRemoteHost document), ssm:TerminateSession,
#     and ec2:DescribeInstances (only when using name/tag discovery).

terraform {
  required_providers {
    postgresql = {
      source = "pinotelio/postgresql-anywhere"
    }
  }
}

provider "postgresql" {
  scheme = "postgres" # tunnels are only supported with the "postgres" scheme

  # host/port are the REAL (private) RDS endpoint. The provider forwards to them.
  host = "mydb.abc123.eu-west-1.rds.amazonaws.com"
  port = 5432

  username = "terraform"
  password = var.db_password

  # TLS terminates at RDS (the tunnel only forwards TCP), so the certificate is
  # RDS's, not localhost's. Use "require" (or "verify-ca" with sslrootcert), not
  # "verify-full".
  sslmode = "require"

  aws_ssm {
    region = "eu-west-1"

    # Bastion discovery, set exactly one of:
    instance_name = "bastion" # by Name tag
    # instance_id   = "i-0123456789abcdef0"          # explicit, takes precedence
    # instance_tags = { Role = "bastion", Env = "prod" }

    # profile    = ""  # optional shared-config profile
    # role_arn   = ""  # optional role to assume before opening the session
    # local_port = 0   # 0 = OS-chosen free loopback port
  }
}

variable "db_password" {
  type      = string
  sensitive = true
}

resource "postgresql_database" "app" {
  name = "app"
}

resource "postgresql_role" "app" {
  name  = "app"
  login = true
}
