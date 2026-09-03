# codetrail infrastructure

**Nothing here has ever been applied.** There is no AWS account behind this
repository, no OIDC role, no state bucket and no `production` environment. What
follows says which claims are verified and by what, and which are not.

## What is verified, with no AWS account

Every one of these runs on a laptop and in CI on every pull request:

```
terraform fmt -check -recursive infra/terraform
terraform init -backend=false && terraform validate     # in infra/terraform
./infra/terraform/offline_plan.sh                       # tfplan + tfplan.json
go test -tags=tfplan ./infra/policy/
```

The stack contains **no data source that calls the AWS API**, which is the one
decision that makes this possible: availability zones are `var.azs` rather than
`data "aws_availability_zones"`, and a single such data source would make
`terraform plan` require credentials and the whole policy suite
account-required. A test asserts it.

Two things about the offline plan that were measured rather than assumed:

- `terraform init -backend=false` is enough for `validate` and **not** enough
  for `plan`, which answers "Backend initialization required". `offline_plan.sh`
  therefore writes a git-ignored `backend_override.tf` naming the local backend.
  The committed configuration still declares the real S3 backend.
- The provider's four `skip_*` flags suppress configure-time *validation*, not
  credential *resolution*. With no `AWS_ACCESS_KEY_ID` at all, plan fails with
  "failed to refresh cached credentials, no EC2 IMDS role found". The dummy
  values the script exports are load-bearing.

## What is not verified

`apply`. Every resource identifier. That a Fargate task with these security
groups can reach `github.com`. That the RDS trust store validates the real
server certificate. The account-required ledger in the P8 plan lists each one
with the command that would close it.

## The state bucket

It is **not created by this stack** — it has to exist before `init` — and it
holds the RDS master password and every secret version **in cleartext**. Its
requirements are in `backend.hcl.example`: encryption, versioning, a public
access block on all four settings, and a policy denying non-TLS requests.

Three artifacts carry secrets, not one: `tfplan`, the `tfplan.json` that
`show -json` produces from it (which carries the assembled `DATABASE_URL`,
password included, as a planned attribute), and state. All three are
git-ignored, none is ever a workflow artifact or a log line, and every policy
assertion reports the offending **key** and never the matched value.

## Variables worth reading before you set them

- **`alb_allowed_cidrs` has no default and the plan fails without it.** What is
  behind this load balancer is an endpoint with no authentication and no rate
  limit that runs `git clone` on a URL a stranger supplied. `["0.0.0.0/0"]` is a
  legitimate answer for a public demo; it just has to be an answer.
- **`azs` must be set when `region` changes.** That is the cost of not having a
  data source, and it is the price of everything in the first section.
