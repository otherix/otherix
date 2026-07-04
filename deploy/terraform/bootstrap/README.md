# Harness bootstrap

One-time operator setup for the AWS test harness. Creates the two persistent
resources every ephemeral harness stand depends on:

- the OpenTofu state bucket (`otherix-tfstate-<account_id>`, versioned,
  AES256-encrypted, public access blocked), and
- the Route53 public hosted zone for `aws.otherix.dev`.

This config uses **local state** (no S3 backend) because it is what creates the
bucket the backend will later use.

## Run once

```
cd deploy/terraform/bootstrap
tofu init
tofu apply
```

## Delegate the subdomain

After apply, take the `zone_name_servers` output and add ONE delegation record
in Cloudflare on the parent `otherix.dev` zone:

```
aws.otherix.dev  NS  -> <zone_name_servers>
```

This hands `aws.otherix.dev` to Route53 so the harness can manage records under it.

## Persistence

The state bucket and hosted zone are persistent. The ephemeral harness never
destroys them; it only references the state bucket:

```
tofu init -backend-config="bucket=<state_bucket>"
```
