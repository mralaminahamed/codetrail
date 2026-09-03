# Workflows

## What has run, and what has not

| Workflow | Trigger | Has it ever run? |
| --- | --- | --- |
| `ci.yml` | push to `trunk`, pull request | Yes, on every pull request. |
| `images.yml` | push, pull request, dispatch | **The build half only.** `push: false` on a pull request builds all three Dockerfiles with no account. **Every push to a registry is unproven: it has never run.** |
| `deploy.yml` | `workflow_dispatch` only | **Never. Not once.** No `terraform apply`, no rollout wait, no deployed smoke test. |

There is no AWS account, no OIDC role, no state bucket and no `production`
environment behind this repository. Nothing above softens that, and nothing
should: the sibling project shipped a Terraform stack and two workflows that had
never been executed once, and said so plainly. This says so too, with the
addition that everything provable without an account **is** proved, on every
pull request — the images build and are asserted, the Terraform plans and is
asserted, and the alert rules are replayed through `promtool`.

## The gates on `deploy.yml`

Three, and only two of them are real today.

1. **`workflow_dispatch` only.** There is deliberately no `push` trigger:
   merging to `trunk` builds images and never touches infrastructure.
2. **`environment: production`.** This is the second gate — dispatch alone only
   proves the actor has repository write access. **An environment with no
   required reviewers is a label, not a gate**, and nobody has configured
   reviewers on an environment that does not exist.
3. **`concurrency` with `queue: max`.** `cancel-in-progress: false` alone does
   not queue: GitHub allows one pending run per group by default and a newly
   queued run *cancels* the pending one it finds, so three dispatches would run
   the first and the third and silently discard the second.

## What deploy.yml does, in order

`terraform plan -out` then `apply` of that saved plan — never
`aws ecs update-service`. The task definitions are Terraform-managed, so
registering one from the CLI puts the live service permanently ahead of state,
and `--force-new-deployment` alone redeploys the *old* image.

Then `aws ecs wait services-stable`, because `terraform apply` returns when ECS
accepts a task definition and not when it runs — without the wait, a
crash-looping service is a green workflow. **Ollama is waited on first and
alone**, because the gateway and the indexer each make one real round trip to
the embedder at boot and exit when it fails.

Then `smoke.sh --target deployed`. Its most valuable assertion is
`edges_resolved > 0`: if the image shipped no Go toolchain, or shipped two, the
repository still indexes, every response is still 200, and every edge is
silently syntactic.

## Secrets and variables it expects

None of these exists. Repository secrets: `AWS_IMAGES_ROLE_ARN`,
`AWS_DEPLOY_ROLE_ARN`, `TF_BACKEND_BUCKET`, `DB_PASSWORD`. Repository variables:
`AWS_REGION`, `ALB_ALLOWED_CIDRS`, `IMAGE_REGISTRY`.

There are **no static AWS credentials** in either workflow; both assume a role
through OIDC.

## Pinning

Every third-party action is pinned to a full commit SHA with its tag in a
trailing comment. `images.yml` would hold a token that can write a registry and
`deploy.yml` would assume a role that can change infrastructure; "whatever `v4`
means today" is not an acceptable input to either.

`.github/actionlint.yaml` carries exactly one ignore, and it is a gap in the
linter rather than in the workflow — see the file.
