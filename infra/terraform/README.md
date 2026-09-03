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

## The account-required ledger

One row per claim this phase could not prove, with the command that would prove
it. A reader with an account can work down it; a reader without one can see the
size of what is unknown. **Nothing in P8's Definition of Done depends on any of
these** — they are the honest remainder.

| # | Unproven claim | Command that proves it |
| --- | --- | --- |
| 1 | The stack applies at all. | `terraform apply -auto-approve` |
| 2 | It is reproducible — nothing was hand-edited in the console. | `terraform plan -detailed-exitcode` — must exit 0 |
| 3 | It is recreatable from nothing. | `terraform destroy -auto-approve && terraform apply -auto-approve && terraform plan -detailed-exitcode` — must exit 0 again |
| 4 | The recreated stack is *equivalent* and not merely present. | `./infra/smoke.sh --target deployed "http://$(terraform output -raw alb_dns_name)"` — before and after the destroy, both passing, including `edges_resolved > 0` |
| 5 | A Fargate task with the indexer's security group can actually reach `github.com`. | Smoke step 6 against the deployed URL |
| 6 | The enumerated DNS egress (VPC `.2` plus `169.254.169.253/32`) is sufficient. | Same; a DNS failure looks like a network outage in the task log |
| 7 | The ALB registers a healthy target against `/ready`. | `aws elbv2 describe-target-health --target-group-arn …` — `State: healthy` |
| 8 | `CREATE EXTENSION vector` succeeds under the RDS master user. | Either binary reaching "postgres ready, schema up to date" in CloudWatch Logs |
| 9 | The RDS trust store validates the real server certificate under `sslmode=verify-full`. | The same log line: `verify-full` fails at boot with an error naming the certificate |
| 10 | The pgvector RDS serves is ≥ 0.5.0, and a minor upgrade has not moved it. | Smoke step 4, or `codetrail_datastore_info` — RDS tops out at 0.8.2 while compose runs 0.8.6, so "the same as dev" is never the expected answer |
| 11 | Images can be pushed to ECR through OIDC. | `images.yml` on a push to `trunk` |
| 12 | `deploy.yml` runs end to end. | One `workflow_dispatch` with an `image_tag` |
| 13 | `queue: max` really queues three dispatches FIFO. | Dispatch three times and watch the run list |
| 14 | The circuit breaker rolls a bad image back rather than stalling. | Deploy a tag that does not exist and watch the service |
| 15 | Prometheus in the VPC discovers both services over Cloud Map. | `observability_enabled = true`, then the targets page |
| 16 | The Ollama image builds with the model baked in. | `docker build -f infra/ollama.Dockerfile .` — **this one needs no account and was still not run**: it starts a server during the build to pull ~274 MB |
| 17 | `manage_master_user_password = true` would keep the password out of state. | The first change to make once an account exists — it needs a data source, which is why it is not the design today |

### Two things in the second column are not account-required and were still not done

Row 16 is a build this machine could have run and did not, and it is listed
rather than quietly omitted. And `edge_enabled` — the middle parked mode in the
README's cost table — is not built at all.
