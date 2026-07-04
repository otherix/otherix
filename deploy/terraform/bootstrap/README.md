# Harness bootstrap

One-time operator setup for the AWS test harness. Creates the two persistent
resources every ephemeral harness stand depends on:

- the OpenTofu state bucket (`otherix-tfstate-<account_id>`, versioned,
  AES256-encrypted, public access blocked), and
- the Route53 public hosted zone for `aws.otherix.dev`.

This config uses **local state** (no S3 backend) because it is what creates the
bucket the backend will later use.

## AWS credentials

OpenTofu's AWS provider uses the AWS SDK credential chain: environment variables
(`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` / `AWS_SESSION_TOKEN`), then an
`~/.aws` profile (`AWS_PROFILE`), then SSO, then an instance role. It does
**not** invoke the `aws` CLI. This bootstrap is run by hand (the `make
harness-*` targets check credentials for you; this step does not), so make sure
one of those sources resolves before you run tofu. A bare `tofu apply` with
nothing in the chain fails with "no valid credential sources".

Any standard setup works, for example `aws sso login` then `export
AWS_PROFILE=<profile>`, a static `~/.aws/credentials` profile, or exporting
credentials into the current shell:

```
eval "$(aws configure export-credentials --profile <profile> --format env)"
```

If your credentials come from a wrapper (a secrets manager, a hardware token),
put that command in front of the export; to avoid leaving credentials resident
in your shell, run tofu in a subshell so the exported variables die with it:

```
( eval "$(aws configure export-credentials --profile <profile> --format env)" && tofu apply )
```

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
