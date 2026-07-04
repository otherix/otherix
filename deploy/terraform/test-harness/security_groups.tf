# Security groups for the harness stand: one per node role plus a rule-less
# blackhole SG used for the network-partition chaos primitive.

resource "aws_security_group" "cp" {
  name        = "otherix-${var.env_name}-cp"
  description = "Control-plane nodes: operator API/SSH plus intra-cluster peering."
  vpc_id      = module.vpc.vpc_id
}

resource "aws_security_group" "gateway" {
  name        = "otherix-${var.env_name}-gateway"
  description = "Gateway nodes: operator SSH/published-LB plus intra-cluster mesh."
  vpc_id      = module.vpc.vpc_id
}

resource "aws_security_group" "agent" {
  name        = "otherix-${var.env_name}-agent"
  description = "Agent nodes: operator SSH plus intra-cluster mesh."
  vpc_id      = module.vpc.vpc_id
}

# blackhole carries NO ingress and NO egress rules: attaching an instance to it
# fully isolates it from the network.
resource "aws_security_group" "blackhole" {
  name        = "otherix-${var.env_name}-blackhole"
  description = "Full-isolation SG for network-partition testing: no ingress, no egress."
  vpc_id      = module.vpc.vpc_id
}

# Allow-all egress for the three functional roles. blackhole intentionally omitted.
resource "aws_vpc_security_group_egress_rule" "cp" {
  security_group_id = aws_security_group.cp.id
  ip_protocol       = "-1"
  cidr_ipv4         = "0.0.0.0/0"
  description       = "Allow all egress."
}

resource "aws_vpc_security_group_egress_rule" "gateway" {
  security_group_id = aws_security_group.gateway.id
  ip_protocol       = "-1"
  cidr_ipv4         = "0.0.0.0/0"
  description       = "Allow all egress."
}

resource "aws_vpc_security_group_egress_rule" "agent" {
  security_group_id = aws_security_group.agent.id
  ip_protocol       = "-1"
  cidr_ipv4         = "0.0.0.0/0"
  description       = "Allow all egress."
}

# Operator-facing ingress: source is each CIDR in var.operator_cidr.

resource "aws_vpc_security_group_ingress_rule" "cp_api_operator" {
  for_each          = toset(var.operator_cidr)
  security_group_id = aws_security_group.cp.id
  cidr_ipv4         = each.value
  ip_protocol       = "tcp"
  from_port         = 8080
  to_port           = 8080
  description       = "Operator control-plane API."
}

resource "aws_vpc_security_group_ingress_rule" "cp_ssh_operator" {
  for_each          = toset(var.operator_cidr)
  security_group_id = aws_security_group.cp.id
  cidr_ipv4         = each.value
  ip_protocol       = "tcp"
  from_port         = 22
  to_port           = 22
  description       = "Operator SSH."
}

resource "aws_vpc_security_group_ingress_rule" "gateway_ssh_operator" {
  for_each          = toset(var.operator_cidr)
  security_group_id = aws_security_group.gateway.id
  cidr_ipv4         = each.value
  ip_protocol       = "tcp"
  from_port         = 22
  to_port           = 22
  description       = "Operator SSH."
}

resource "aws_vpc_security_group_ingress_rule" "agent_ssh_operator" {
  for_each          = toset(var.operator_cidr)
  security_group_id = aws_security_group.agent.id
  cidr_ipv4         = each.value
  ip_protocol       = "tcp"
  from_port         = 22
  to_port           = 22
  description       = "Operator SSH."
}

resource "aws_vpc_security_group_ingress_rule" "gateway_published_operator" {
  for_each          = toset(var.operator_cidr)
  security_group_id = aws_security_group.gateway.id
  cidr_ipv4         = each.value
  ip_protocol       = "tcp"
  from_port         = var.published_port_from
  to_port           = var.published_port_to
  description       = "Operator published load-balancer ports."
}

resource "aws_vpc_security_group_ingress_rule" "gateway_ingress_operator" {
  for_each          = toset(var.operator_cidr)
  security_group_id = aws_security_group.gateway.id
  cidr_ipv4         = each.value
  ip_protocol       = "tcp"
  from_port         = 9444
  to_port           = 9444
  description       = "Operator/external ingress splicer connect plane."
}

# Intra-cluster ingress: source is a referenced security group, never a CIDR.

resource "aws_vpc_security_group_ingress_rule" "cp_agent_join" {
  security_group_id            = aws_security_group.cp.id
  referenced_security_group_id = aws_security_group.agent.id
  ip_protocol                  = "tcp"
  from_port                    = 8443
  to_port                      = 8443
  description                  = "Agent-to-CP mutual-TLS join/heartbeat."
}

resource "aws_vpc_security_group_ingress_rule" "cp_gateway_join" {
  security_group_id            = aws_security_group.cp.id
  referenced_security_group_id = aws_security_group.gateway.id
  ip_protocol                  = "tcp"
  from_port                    = 8443
  to_port                      = 8443
  description                  = "Gateway-to-CP mutual-TLS join/heartbeat."
}

resource "aws_vpc_security_group_ingress_rule" "cp_cluster_join" {
  security_group_id            = aws_security_group.cp.id
  referenced_security_group_id = aws_security_group.cp.id
  ip_protocol                  = "tcp"
  from_port                    = 8443
  to_port                      = 8443
  description                  = "CP replica cluster-join redemption (/v1/cluster/join)."
}

resource "aws_vpc_security_group_ingress_rule" "cp_peer_raft" {
  security_group_id            = aws_security_group.cp.id
  referenced_security_group_id = aws_security_group.cp.id
  ip_protocol                  = "tcp"
  from_port                    = 2380
  to_port                      = 2380
  description                  = "Embedded etcd peer traffic between CP replicas."
}

resource "aws_vpc_security_group_ingress_rule" "agent_cp_callback" {
  security_group_id            = aws_security_group.agent.id
  referenced_security_group_id = aws_security_group.cp.id
  ip_protocol                  = "tcp"
  from_port                    = 9443
  to_port                      = 9443
  description                  = "CP-to-agent control channel."
}

# The gateway advertises its PUBLIC host on --advertised-endpoint (:9443), and
# SG-reference rules only match private-IP intra-VPC traffic, so this rule is
# latent today: nothing dials a gateway on 9443 (gateways host no VMs). If a
# CP-to-gateway control call over the advertised endpoint is ever added it would
# need an operator/public-CIDR rule instead.
resource "aws_vpc_security_group_ingress_rule" "gateway_cp_callback" {
  security_group_id            = aws_security_group.gateway.id
  referenced_security_group_id = aws_security_group.cp.id
  ip_protocol                  = "tcp"
  from_port                    = 9443
  to_port                      = 9443
  description                  = "CP-to-gateway control channel."
}

resource "aws_vpc_security_group_ingress_rule" "gateway_agent_broker" {
  security_group_id            = aws_security_group.gateway.id
  referenced_security_group_id = aws_security_group.agent.id
  ip_protocol                  = "tcp"
  from_port                    = 9444
  to_port                      = 9444
  description                  = "Agent-to-gateway broker channel."
}

resource "aws_vpc_security_group_ingress_rule" "gateway_gateway_broker" {
  security_group_id            = aws_security_group.gateway.id
  referenced_security_group_id = aws_security_group.gateway.id
  ip_protocol                  = "tcp"
  from_port                    = 9444
  to_port                      = 9444
  description                  = "Gateway-to-gateway broker channel."
}

# Mesh: WireGuard overlay plus the two migration port bands connect the
# {agent, gateway} pair in all four directions. Flattened over range x (dest, src).
locals {
  mesh_sg_ids = {
    agent   = aws_security_group.agent.id
    gateway = aws_security_group.gateway.id
  }

  mesh_ranges = {
    wireguard   = { proto = "udp", from = 51820, to = 51820 }
    migration_a = { proto = "tcp", from = 49152, to = 49251 }
    migration_b = { proto = "tcp", from = 49252, to = 49351 }
  }

  mesh_roles = ["agent", "gateway"]

  mesh_rules = merge([
    for range_name, r in local.mesh_ranges : {
      for pair in setproduct(local.mesh_roles, local.mesh_roles) :
      "${range_name}-${pair[0]}-from-${pair[1]}" => {
        dest  = pair[0]
        src   = pair[1]
        proto = r.proto
        from  = r.from
        to    = r.to
      }
    }
  ]...)
}

resource "aws_vpc_security_group_ingress_rule" "mesh" {
  for_each                     = local.mesh_rules
  security_group_id            = local.mesh_sg_ids[each.value.dest]
  referenced_security_group_id = local.mesh_sg_ids[each.value.src]
  ip_protocol                  = each.value.proto
  from_port                    = each.value.from
  to_port                      = each.value.to
  description                  = "Overlay/migration mesh: ${each.value.src} to ${each.value.dest}."
}
