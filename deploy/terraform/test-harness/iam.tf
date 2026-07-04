# IAM instance profiles for the three node roles. Each role assumes via EC2 and
# reaches SSM Parameter Store for the bootstrap-token rendezvous. The CP role
# writes tokens and reads its own admin/jwt params (wildcard over the env prefix);
# the agent and gateway roles are scoped to the three token params ONLY, so a
# compromised worker node cannot read jwt_secret or the admin credentials.

data "aws_caller_identity" "current" {}

# Random suffix keeps globally-scoped IAM names from colliding across stands that
# happen to reuse an env_name.
resource "random_id" "iam_suffix" {
  byte_length = 4
}

locals {
  ssm_prefix_arn = "arn:aws:ssm:${var.region}:${data.aws_caller_identity.current.account_id}:parameter/otherix/${var.env_name}"
  iam_suffix     = random_id.iam_suffix.hex

  # The three bootstrap-token params agent and gateway nodes are allowed to read.
  token_param_arns = [
    "${local.ssm_prefix_arn}/join_token_node",
    "${local.ssm_prefix_arn}/join_token_gateway",
    "${local.ssm_prefix_arn}/ca_fingerprint",
  ]
}

# Shared EC2 assume-role trust policy for all three roles.
data "aws_iam_policy_document" "ec2_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

# CP: read/write the full env-scoped param prefix (writes bootstrap tokens, reads
# its own admin/jwt params).
data "aws_iam_policy_document" "cp_ssm" {
  statement {
    effect = "Allow"
    actions = [
      "ssm:PutParameter",
      "ssm:GetParameter",
      "ssm:GetParameters",
    ]
    resources = ["${local.ssm_prefix_arn}/*"]
  }
}

# Agent and gateway: read ONLY the three bootstrap-token params. Deliberately NOT
# a wildcard over the env prefix, so worker nodes cannot read jwt_secret or the
# admin credentials.
data "aws_iam_policy_document" "worker_ssm" {
  statement {
    effect = "Allow"
    actions = [
      "ssm:GetParameter",
      "ssm:GetParameters",
    ]
    resources = local.token_param_arns
  }
}

# SSM core messaging + RunCommand permissions, replicating
# AmazonSSMManagedInstanceCore but WITHOUT ssm:GetParameter / ssm:GetParameters -
# the managed policy grants those on Resource "*", so attaching it to a worker
# role would let it read every SSM parameter (including jwt_secret and the admin
# credentials), defeating the scoped worker_ssm policy. This trimmed version keeps
# SSM RunCommand working (used by the chaos latency primitive) while leaving
# parameter access solely to the explicit cp_ssm / worker_ssm policies.
data "aws_iam_policy_document" "ssm_core" {
  statement {
    effect = "Allow"
    actions = [
      "ssm:DescribeAssociation",
      "ssm:GetDeployablePatchSnapshotForInstance",
      "ssm:GetDocument",
      "ssm:DescribeDocument",
      "ssm:GetManifest",
      "ssm:ListAssociations",
      "ssm:ListInstanceAssociations",
      "ssm:PutInventory",
      "ssm:PutComplianceItems",
      "ssm:PutConfigurePackageResult",
      "ssm:UpdateAssociationStatus",
      "ssm:UpdateInstanceAssociationStatus",
      "ssm:UpdateInstanceInformation",
    ]
    resources = ["*"]
  }
  statement {
    effect = "Allow"
    actions = [
      "ssmmessages:CreateControlChannel",
      "ssmmessages:CreateDataChannel",
      "ssmmessages:OpenControlChannel",
      "ssmmessages:OpenDataChannel",
    ]
    resources = ["*"]
  }
  statement {
    effect = "Allow"
    actions = [
      "ec2messages:AcknowledgeMessage",
      "ec2messages:DeleteMessage",
      "ec2messages:FailMessage",
      "ec2messages:GetEndpoint",
      "ec2messages:GetMessages",
      "ec2messages:SendReply",
    ]
    resources = ["*"]
  }
}

# CP role and profile.
resource "aws_iam_role" "cp" {
  name               = "otherix-${var.env_name}-cp-${local.iam_suffix}"
  assume_role_policy = data.aws_iam_policy_document.ec2_assume.json
}

resource "aws_iam_role_policy" "cp_ssm" {
  name   = "ssm-params"
  role   = aws_iam_role.cp.id
  policy = data.aws_iam_policy_document.cp_ssm.json
}

resource "aws_iam_role_policy" "cp_ssm_core" {
  name   = "ssm-core"
  role   = aws_iam_role.cp.id
  policy = data.aws_iam_policy_document.ssm_core.json
}

resource "aws_iam_instance_profile" "cp" {
  name = "otherix-${var.env_name}-cp-${local.iam_suffix}"
  role = aws_iam_role.cp.name
}

# Agent role and profile.
resource "aws_iam_role" "agent" {
  name               = "otherix-${var.env_name}-agent-${local.iam_suffix}"
  assume_role_policy = data.aws_iam_policy_document.ec2_assume.json
}

resource "aws_iam_role_policy" "agent_ssm" {
  name   = "ssm-token-params"
  role   = aws_iam_role.agent.id
  policy = data.aws_iam_policy_document.worker_ssm.json
}

resource "aws_iam_role_policy" "agent_ssm_core" {
  name   = "ssm-core"
  role   = aws_iam_role.agent.id
  policy = data.aws_iam_policy_document.ssm_core.json
}

resource "aws_iam_instance_profile" "agent" {
  name = "otherix-${var.env_name}-agent-${local.iam_suffix}"
  role = aws_iam_role.agent.name
}

# Gateway role and profile.
resource "aws_iam_role" "gateway" {
  name               = "otherix-${var.env_name}-gateway-${local.iam_suffix}"
  assume_role_policy = data.aws_iam_policy_document.ec2_assume.json
}

resource "aws_iam_role_policy" "gateway_ssm" {
  name   = "ssm-token-params"
  role   = aws_iam_role.gateway.id
  policy = data.aws_iam_policy_document.worker_ssm.json
}

resource "aws_iam_role_policy" "gateway_ssm_core" {
  name   = "ssm-core"
  role   = aws_iam_role.gateway.id
  policy = data.aws_iam_policy_document.ssm_core.json
}

resource "aws_iam_instance_profile" "gateway" {
  name = "otherix-${var.env_name}-gateway-${local.iam_suffix}"
  role = aws_iam_role.gateway.name
}
