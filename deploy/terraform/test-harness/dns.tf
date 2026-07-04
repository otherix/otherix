# Persistent public zone, created once by the bootstrap config.
data "aws_route53_zone" "public" {
  name         = "aws.otherix.dev."
  private_zone = false
}

# Public round-robin A records for the operator and client surfaces.
resource "aws_route53_record" "cp_public" {
  zone_id = data.aws_route53_zone.public.zone_id
  name    = "cp.${local.fqdn_public}"
  type    = "A"
  ttl     = 60
  records = [for k, v in aws_instance.cp : v.public_ip]
}

resource "aws_route53_record" "gw_public" {
  zone_id = data.aws_route53_zone.public.zone_id
  name    = "gw.${local.fqdn_public}"
  type    = "A"
  ttl     = 60
  records = [for k, v in aws_instance.gateway : v.public_ip]
}

# Per-stand private zone, associated with this VPC, for the cluster-internal names.
resource "aws_route53_zone" "internal" {
  name = local.fqdn_internal
  vpc {
    vpc_id = module.vpc.vpc_id
  }
}

# Round-robin over the CP private IPs. Agents heartbeat here, so it survives CP-0 loss.
resource "aws_route53_record" "cp_internal" {
  zone_id = aws_route53_zone.internal.zone_id
  name    = "cp.${local.fqdn_internal}"
  type    = "A"
  ttl     = 60
  records = [for k, v in aws_instance.cp : v.private_ip]
}

# Single record for the cluster-join target (CP-0 private IP).
resource "aws_route53_record" "cp0_internal" {
  zone_id = aws_route53_zone.internal.zone_id
  name    = "cp-0.${local.fqdn_internal}"
  type    = "A"
  ttl     = 60
  records = [aws_instance.cp["cp-0"].private_ip]
}
