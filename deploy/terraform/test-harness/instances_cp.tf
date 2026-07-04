# Control-plane instances. cp-0 seeds a single-member etcd cluster and mints the
# fleet join tokens; cp-1 and cp-2 join it via the SSM token rendezvous. The
# per-instance bootstrap lives in the cloud-init templates under cloud-init/.

data "aws_ssm_parameter" "ubuntu_arm64" {
  name = "/aws/service/canonical/ubuntu/server/24.04/stable/current/arm64/hvm/ebs-gp3/ami-id"
}

locals {
  cp_nodes = { "cp-0" = local.cp_azs[0], "cp-1" = local.cp_azs[1], "cp-2" = local.cp_azs[2] }
}

resource "aws_instance" "cp" {
  for_each          = local.cp_nodes
  ami               = data.aws_ssm_parameter.ubuntu_arm64.value
  instance_type     = var.cp_instance_type
  subnet_id         = module.vpc.public_subnets[local.az_index[each.value]]
  availability_zone = each.value

  vpc_security_group_ids      = [aws_security_group.cp.id]
  iam_instance_profile        = aws_iam_instance_profile.cp.name
  associate_public_ip_address = true

  # cp-0 is the bootstrap seed and a formation SPOF, so it is always on-demand;
  # cp-1/cp-2 follow var.on_demand_cp.
  dynamic "instance_market_options" {
    for_each = (var.on_demand_cp || each.key == "cp-0") ? [] : [1]
    content { market_type = "spot" }
  }

  user_data = templatefile(
    each.key == "cp-0" ? "${path.module}/cloud-init/cp0.yaml.tftpl" : "${path.module}/cloud-init/cp-join.yaml.tftpl",
    {
      region          = var.region
      env_name        = var.env_name
      otherix_version = var.otherix_version
      fqdn_public     = local.fqdn_public
      fqdn_internal   = local.fqdn_internal
      node_name       = each.key
      cp_api_port     = var.cp_api_port
    }
  )

  # cloud-init is the whole node bootstrap, and its runcmd only runs on first
  # boot of a given instance-id. A user_data edit must therefore recreate the
  # instance, not just update the attribute in place (which never re-runs
  # cloud-init). Without this a cloud-init fix silently no-ops on re-apply.
  user_data_replace_on_change = true

  tags = {
    Name           = "otherix-${var.env_name}-${each.key}"
    "otherix:role" = "cp"
  }
}
