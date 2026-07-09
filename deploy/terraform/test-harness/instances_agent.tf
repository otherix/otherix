# Bare-metal agent nodes. Each assembles its local NVMe instance-store into a
# storage pool and bootstraps against the control plane using its private IP as
# the advertised endpoint. Placed in private subnets (one in 1a, one in 1c).
#
# Each agent is a per-node EC2 Fleet (type=instant, target 1) rather than a plain
# spot aws_instance: a single (type, AZ) spot request has no fallback and is what
# hits InsufficientInstanceCapacity when the pool runs dry. The fleet spreads the
# one instance over var.agent_instance_pool (interchangeable arm64 metals) in the
# node's pinned AZ and lets price-capacity-optimized pick the cheapest type with
# capacity. One fleet PER node (keyed agent-0/agent-1) keeps the stable handle the
# chaos scripts address by index (dev/chaos/*.sh -> instance_ids["agent-N"]) while
# still giving the node a capacity pool; Otherix node identity stays agent-<index>.

locals {
  agent_nodes = { "agent-0" = local.agent_azs[0], "agent-1" = local.agent_azs[1] }
}

resource "aws_launch_template" "agent" {
  for_each    = local.agent_nodes
  name_prefix = "otherix-${var.env_name}-${each.key}-"
  image_id    = data.aws_ssm_parameter.ubuntu_arm64.value

  iam_instance_profile {
    name = aws_iam_instance_profile.agent.name
  }

  vpc_security_group_ids = [aws_security_group.agent.id]

  # The AMI enforces IMDSv2 and the cloud-init fetches a session token; pin it
  # here too so a pool member launched from this template can never fall back to
  # IMDSv1 (the private-IP read would then fail the bootstrap).
  metadata_options {
    http_endpoint = "enabled"
    http_tokens   = "required"
  }

  user_data = base64encode(templatefile("${path.module}/cloud-init/agent.yaml.tftpl", {
    region          = var.region
    env_name        = var.env_name
    otherix_version = var.otherix_version
    fqdn_internal   = local.fqdn_internal
    index           = trimprefix(each.key, "agent-")
    raid0_pool      = var.raid0_pool
  }))

  tag_specifications {
    resource_type = "instance"
    tags = {
      Name           = "otherix-${var.env_name}-${each.key}"
      "otherix:role" = "agent"
    }
  }
}

resource "aws_ec2_fleet" "agent" {
  for_each = local.agent_nodes

  # instant returns the launched instance in state synchronously (so IPs/DNS can
  # reference it) and does NOT replace an interrupted instance - which is exactly
  # what the chaos suite wants (a killed node stays dead until re-applied).
  type                = "instant"
  terminate_instances = true

  launch_template_config {
    launch_template_specification {
      launch_template_id = aws_launch_template.agent[each.key].id
      version            = aws_launch_template.agent[each.key].latest_version
    }

    # One override per pool type, all in the node's pinned AZ/subnet. The fleet
    # scores these and launches the cheapest with capacity.
    dynamic "override" {
      for_each = var.agent_instance_pool
      content {
        instance_type     = override.value
        subnet_id         = module.vpc.private_subnets[local.az_index[each.value]]
        availability_zone = each.value
      }
    }
  }

  spot_options {
    allocation_strategy = "price-capacity-optimized"
  }

  target_capacity_specification {
    total_target_capacity = 1
    # on_demand_agent flips the single unit to on-demand (the manual escape hatch
    # if the whole spot pool is dry); default is spot across the pool.
    default_target_capacity_type = var.on_demand_agent ? "on-demand" : "spot"
  }

  # A cloud-init edit bumps the launch-template version; recreate the fleet so the
  # replacement instance actually boots the updated bootstrap (an in-place fleet
  # never re-runs cloud-init on the existing instance) - the aws_instance form
  # relied on user_data_replace_on_change for the same guarantee.
  lifecycle {
    replace_triggered_by = [aws_launch_template.agent[each.key].latest_version]
  }

  tags = {
    Name           = "otherix-${var.env_name}-${each.key}-fleet"
    "otherix:role" = "agent"
  }
}

# Resolve each fleet's single launched instance so IPs and ids stay addressable by
# the stable agent-0/agent-1 key (outputs, chaos). one() asserts exactly one id.
data "aws_instance" "agent" {
  for_each    = aws_ec2_fleet.agent
  instance_id = one(each.value.fleet_instance_set[0].instance_ids)
}
