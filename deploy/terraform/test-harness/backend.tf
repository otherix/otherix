# The `bucket` is supplied at init time by the operator:
#   tofu init -backend-config="bucket=otherix-tfstate-<account>"
# (the bucket is created once by deploy/terraform/bootstrap). Workspaces isolate
# each stand under env:/<env>/test-harness/terraform.tfstate.
terraform {
  backend "s3" {
    key          = "test-harness/terraform.tfstate"
    region       = "eu-north-1"
    use_lockfile = true
    encrypt      = true
  }
}
