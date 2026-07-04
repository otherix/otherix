output "state_bucket" {
  description = "Name of the persistent OpenTofu state bucket used by every harness stand."
  value       = aws_s3_bucket.tfstate.id
}

output "zone_id" {
  description = "Route53 hosted zone id for aws.otherix.dev."
  value       = aws_route53_zone.harness.zone_id
}

output "zone_name_servers" {
  description = "Authoritative name servers for the delegation NS record on the parent otherix.dev zone."
  value       = aws_route53_zone.harness.name_servers
}
