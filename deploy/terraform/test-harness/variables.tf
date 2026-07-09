variable "region" {
  description = "AWS region the harness stand is provisioned in."
  type        = string
  default     = "eu-north-1"
}

variable "env_name" {
  description = "Stand name; isolates one harness deployment (used in tags, DNS, workspace)."
  type        = string
  default     = "test"
  validation {
    condition     = can(regex("^[a-z0-9-]{1,20}$", var.env_name))
    error_message = "env_name must be lowercase alphanumeric plus hyphen, length 1..20."
  }
}

variable "operator_cidr" {
  description = "Source CIDRs for operator-facing rules; default open (unset = no restriction)."
  type        = list(string)
  default     = ["0.0.0.0/0"]
}

variable "cp_api_port" {
  description = "Port the control-plane user API listens on and the operator CLI targets. 443 is omitted from the CLI URL (https implies it)."
  type        = number
  default     = 443
}

variable "otherix_version" {
  description = "The .deb release tag installed by cloud-init (without the leading v). harness-up resolves the latest release when unset; empty is accepted so teardown never needs a value."
  type        = string
  default     = ""
}

variable "cp_instance_type" {
  description = "EC2 instance type for control-plane nodes."
  type        = string
  default     = "m6g.large"
}

variable "agent_instance_pool" {
  description = "Ordered spot pool of interchangeable bare-metal agent types (arm64, nested-KVM, local NVMe). Each per-node fleet picks the cheapest type with capacity (price-capacity-optimized), diversifying away the single-pool exhaustion that causes InsufficientInstanceCapacity."
  type        = list(string)
  default     = ["c6gd.metal", "c7gd.metal", "m6gd.metal", "m7gd.metal", "r6gd.metal"]
  validation {
    condition     = length(var.agent_instance_pool) > 0
    error_message = "agent_instance_pool must list at least one instance type."
  }
}

variable "gateway_instance_type" {
  description = "EC2 instance type for gateway nodes. A single .large arm64 spot instance already scores SPS ~9, so gateways need no capacity pool; the tiny t4g.medium had almost none (SPS 1-3)."
  type        = string
  default     = "m6g.large"
}

variable "on_demand_cp" {
  description = "Use on-demand (not spot) capacity for control-plane nodes."
  type        = bool
  default     = false
}

variable "on_demand_agent" {
  description = "Use on-demand (not spot) capacity for agent nodes."
  type        = bool
  default     = false
}

variable "on_demand_gateway" {
  description = "Use on-demand (not spot) capacity for gateway nodes."
  type        = bool
  default     = false
}

variable "raid0_pool" {
  description = "Combine agent NVMe disks into one RAID0 storage pool (false = two separate pools)."
  type        = bool
  default     = true
}

variable "published_port_from" {
  description = "Low bound of the gateway published-LB ingress band the security group opens."
  type        = number
  default     = 30000
}

variable "published_port_to" {
  description = "High bound of the gateway published-LB ingress band the security group opens."
  type        = number
  default     = 32767
}

variable "bootstrap_admin_username" {
  description = "Bootstrap admin account username seeded into the control plane."
  type        = string
  default     = "admin"
}

variable "bootstrap_admin_password" {
  description = "Bootstrap admin account password (empty = random-generated at apply time)."
  type        = string
  sensitive   = true
  default     = ""
}

locals {
  azs         = ["eu-north-1a", "eu-north-1b", "eu-north-1c"]
  cp_azs      = ["eu-north-1a", "eu-north-1b", "eu-north-1c"]
  agent_azs   = ["eu-north-1a", "eu-north-1c"]
  gateway_azs = ["eu-north-1b", "eu-north-1c"]
  az_index    = { for i, az in local.azs : az => i }

  domain        = "aws.otherix.dev"
  fqdn_public   = "${var.env_name}.${local.domain}"
  fqdn_internal = "internal.${var.env_name}.${local.domain}"
}
