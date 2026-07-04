locals {
  gateway_nodes = { "gw-0" = "eu-north-1b", "gw-1" = "eu-north-1c" }
}

resource "aws_instance" "gateway" {
  for_each          = local.gateway_nodes
  ami               = data.aws_ssm_parameter.ubuntu_arm64.value
  instance_type     = var.gateway_instance_type
  subnet_id         = module.vpc.public_subnets[local.az_index[each.value]]
  availability_zone = each.value

  vpc_security_group_ids      = [aws_security_group.gateway.id]
  iam_instance_profile        = aws_iam_instance_profile.gateway.name
  associate_public_ip_address = true

  dynamic "instance_market_options" {
    for_each = var.on_demand_gateway ? [] : [1]
    content { market_type = "spot" }
  }

  user_data = templatefile("${path.module}/cloud-init/gateway.yaml.tftpl", {
    region          = var.region
    env_name        = var.env_name
    otherix_version = var.otherix_version
    fqdn_internal   = local.fqdn_internal
    index           = trimprefix(each.key, "gw-")
  })

  tags = {
    Name           = "otherix-${var.env_name}-${each.key}"
    "otherix:role" = "gateway"
  }
}
