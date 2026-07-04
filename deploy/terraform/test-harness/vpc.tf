module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "~> 5.0"

  name = "otherix-${var.env_name}"
  cidr = "10.60.0.0/16"

  azs             = local.azs
  public_subnets  = ["10.60.0.0/20", "10.60.16.0/20", "10.60.32.0/20"]
  private_subnets = ["10.60.128.0/20", "10.60.144.0/20", "10.60.160.0/20"]

  enable_nat_gateway   = true
  single_nat_gateway   = true
  enable_dns_hostnames = true
  enable_dns_support   = true
}
