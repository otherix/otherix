# SSM Parameter Store entries under /otherix/${var.env_name}/.
#
# Two kinds of parameters live here:
#   1. Terraform-seeded secrets (jwt_secret, admin_username, admin_password).
#      These are authored at apply time and owned by Terraform.
#   2. Runtime-written token placeholders (join_token_*, ca_fingerprint).
#      CP-0 overwrites these at boot via PutParameter --overwrite. Terraform
#      only seeds a "pending" placeholder and then ignores value drift, so the
#      runtime write does not fight Terraform on the next apply, yet the params
#      remain Terraform-managed and are removed by tofu destroy.

resource "random_password" "jwt" {
  length  = 48
  special = false
}

# Used only when the operator did not supply var.bootstrap_admin_password.
resource "random_password" "admin" {
  length  = 24
  special = false
}

resource "aws_ssm_parameter" "jwt_secret" {
  name  = "/otherix/${var.env_name}/jwt_secret"
  type  = "SecureString"
  value = random_password.jwt.result
}

resource "aws_ssm_parameter" "admin_username" {
  name  = "/otherix/${var.env_name}/admin_username"
  type  = "SecureString"
  value = var.bootstrap_admin_username
}

resource "aws_ssm_parameter" "admin_password" {
  name = "/otherix/${var.env_name}/admin_password"
  type = "SecureString"
  # coalesce skips the empty-string default, so an unset password becomes the
  # generated one.
  value = coalesce(var.bootstrap_admin_password, random_password.admin.result)
}

resource "aws_ssm_parameter" "join_token_cluster" {
  name  = "/otherix/${var.env_name}/join_token_cluster"
  type  = "SecureString"
  value = "pending"

  lifecycle {
    ignore_changes = [value]
  }
}

resource "aws_ssm_parameter" "join_token_node" {
  name  = "/otherix/${var.env_name}/join_token_node"
  type  = "SecureString"
  value = "pending"

  lifecycle {
    ignore_changes = [value]
  }
}

resource "aws_ssm_parameter" "join_token_gateway" {
  name  = "/otherix/${var.env_name}/join_token_gateway"
  type  = "SecureString"
  value = "pending"

  lifecycle {
    ignore_changes = [value]
  }
}

resource "aws_ssm_parameter" "ca_fingerprint" {
  name  = "/otherix/${var.env_name}/ca_fingerprint"
  type  = "SecureString"
  value = "pending"

  lifecycle {
    ignore_changes = [value]
  }
}
