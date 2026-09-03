# ECR and not GHCR. This repository is private, GHCR packages inherit repository
# visibility, and a private GHCR image fails a Fargate pull with
# CannotPullContainerError while the circuit breaker rolls the apply back. ECR
# is pulled by the execution role through IAM, so there is no registry
# credential anywhere and the whole class of bug does not exist.
resource "aws_ecr_repository" "svc" {
  for_each = local.images

  name                 = "${var.name}/${each.key}"
  image_tag_mutability = "MUTABLE"

  image_scanning_configuration {
    scan_on_push = true
  }
}

# Keeps the registry from growing without bound on a stack whose images are
# rebuilt on every push to trunk.
resource "aws_ecr_lifecycle_policy" "svc" {
  for_each   = aws_ecr_repository.svc
  repository = each.value.name

  policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "Keep the last 20 images"
      selection    = { tagStatus = "any", countType = "imageCountMoreThan", countNumber = 20 }
      action       = { type = "expire" }
    }]
  })
}
