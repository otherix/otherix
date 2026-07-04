output "cp_url" {
  description = "Operator CLI target for the control plane."
  value       = "https://cp.${local.fqdn_public}${var.cp_api_port == 443 ? "" : ":${var.cp_api_port}"}"
}

output "gateway_fqdn" {
  description = "Published-LB client endpoint."
  value       = "gw.${local.fqdn_public}"
}

output "instance_ids" {
  description = "Map from node name to EC2 instance id across all roles."
  value = merge(
    { for k, v in aws_instance.cp : k => v.id },
    { for k, v in aws_instance.agent : k => v.id },
    { for k, v in aws_instance.gateway : k => v.id },
  )
}

output "cp_public_ips" {
  description = "Map from control-plane node name to public IP."
  value       = { for k, v in aws_instance.cp : k => v.public_ip }
}

output "agent_private_ips" {
  description = "Map from agent node name to private IP."
  value       = { for k, v in aws_instance.agent : k => v.private_ip }
}

output "role_security_group_ids" {
  description = "Per-role security group ids used by chaos heal to restore a node."
  value = {
    cp      = aws_security_group.cp.id
    agent   = aws_security_group.agent.id
    gateway = aws_security_group.gateway.id
  }
}

output "blackhole_sg_id" {
  description = "Security group used by chaos partition to isolate a node."
  value       = aws_security_group.blackhole.id
}
