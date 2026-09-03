data "aws_iam_policy_document" "execution_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

# The EXECUTION role, which the ECS agent holds and the container does not. It
# pulls the image and reads the secrets before the process starts; nothing
# inside the container can use it. That distinction is the whole reason the
# task_role_arn below is null and this is not.
resource "aws_iam_role" "execution" {
  name               = "${var.name}-execution"
  assume_role_policy = data.aws_iam_policy_document.execution_assume.json
}

data "aws_iam_policy_document" "execution" {
  # Every statement names a resource ATTRIBUTE and never a hand-built ARN
  # string. skip_requesting_account_id does not fake an account, it leaves it
  # empty, so `arn:aws:logs:${region}:${account}:...` would plan incorrectly and
  # silently with no credentials — which is exactly the offline plan this whole
  # phase rests on.
  statement {
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"] # A global action with no resource of its own.
  }

  statement {
    actions = [
      "ecr:BatchCheckLayerAvailability",
      "ecr:GetDownloadUrlForLayer",
      "ecr:BatchGetImage",
    ]
    resources = [for r in aws_ecr_repository.svc : r.arn]
  }

  statement {
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents"]
    resources = [for g in aws_cloudwatch_log_group.svc : "${g.arn}:*"]
  }

  # Iterates the same map the secrets are created from, so a secret added later
  # is readable without a second policy that can drift. The sibling project's
  # registry-credentials bug was a secret created outside that map.
  statement {
    actions   = ["secretsmanager:GetSecretValue"]
    resources = [for s in aws_secretsmanager_secret.app : s.arn]
  }
}

resource "aws_iam_role_policy" "execution" {
  name   = "${var.name}-execution"
  role   = aws_iam_role.execution.id
  policy = data.aws_iam_policy_document.execution.json
}

resource "aws_cloudwatch_log_group" "svc" {
  for_each          = local.services
  name              = "/ecs/${var.name}/${each.key}"
  retention_in_days = var.log_retention_days
}
