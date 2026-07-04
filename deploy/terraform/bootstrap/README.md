# Harness bootstrap

One-time operator setup for the AWS test harness. Creates the two persistent
resources every ephemeral harness stand depends on:

- the OpenTofu state bucket (`otherix-tfstate-<account_id>`, versioned,
  AES256-encrypted, public access blocked), and
- the Route53 public hosted zone for `aws.otherix.dev`.

This config uses **local state** (no S3 backend) because it is what creates the
bucket the backend will later use.

## AWS credentials

OpenTofu's AWS provider uses the AWS SDK credential chain (environment
variables, then `~/.aws/credentials`, then SSO / IMDS). It does **not** invoke
the `aws` CLI, so a 1Password `aws` shell-plugin wrapper is bypassed and never
prompts during `tofu apply`. Unlike the `make harness-*` targets (which inject
credentials automatically), this bootstrap is run by hand, so you must put valid
credentials in the environment first.

With the 1Password `aws` shell plugin, export session credentials into the
current shell before running tofu:

```
eval "$(op plugin run -- aws configure export-credentials --format env)"
```

This triggers the 1Password approval once and sets `AWS_ACCESS_KEY_ID` /
`AWS_SECRET_ACCESS_KEY` (and `AWS_SESSION_TOKEN` for role/SSO items). To avoid
leaving long-lived credentials resident in your shell, run tofu in a subshell so
the exported variables die with it:

```
( eval "$(op plugin run -- aws configure export-credentials --format env)" && tofu apply )
```

Without 1Password, use any standard mechanism (`aws sso login` + `AWS_PROFILE`,
a static `~/.aws/credentials` profile, or exported `AWS_*` variables). A bare
`tofu apply` with no credentials in the chain fails with "no valid credential
sources".

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
