# Bare-metal agent nodes. Each assembles its local NVMe instance-store into a
# storage pool and bootstraps against the control plane using its private IP as
# the advertised endpoint. Placed in private subnets (one in 1a, one in 1c).

locals {
  agent_nodes = { "agent-0" = local.agent_azs[0], "agent-1" = local.agent_azs[1] }
}

resource "aws_instance" "agent" {
  for_each          = local.agent_nodes
  ami               = data.aws_ssm_parameter.ubuntu_arm64.value
  instance_type     = var.agent_instance_type
  subnet_id         = module.vpc.private_subnets[local.az_index[each.value]]
  availability_zone = each.value

  vpc_security_group_ids      = [aws_security_group.agent.id]
  iam_instance_profile        = aws_iam_instance_profile.agent.name
  associate_public_ip_address = false

  dynamic "instance_market_options" {
    for_each = var.on_demand_agent ? [] : [1]
    content { market_type = "spot" }
  }

  user_data = templatefile("${path.module}/cloud-init/agent.yaml.tftpl", {
    region          = var.region
    env_name        = var.env_name
    otherix_version = var.otherix_version
    fqdn_internal   = local.fqdn_internal
    index           = trimprefix(each.key, "agent-")
    raid0_pool      = var.raid0_pool
  })

  tags = {
    Name           = "otherix-${var.env_name}-${each.key}"
    "otherix:role" = "agent"
  }
}
