resource "aws_ecs_cluster" "main" {
  name = var.name
}

# Cloud Map, so Prometheus can find tasks whose addresses change on every
# replacement, and so the gateway and the indexer can name the embedder.
resource "aws_service_discovery_private_dns_namespace" "main" {
  name = "${var.name}.local"
  vpc  = aws_vpc.main.id
}

resource "aws_service_discovery_service" "svc" {
  for_each = local.services

  name = each.key

  dns_config {
    namespace_id = aws_service_discovery_private_dns_namespace.main.id
    dns_records {
      ttl  = 10
      type = "A"
    }
    routing_policy = "MULTIVALUE"
  }

  # Custom health status, so ECS reports task health to Cloud Map rather than
  # Route 53 probing an instance it cannot reach. failure_threshold is
  # deliberately unset: the provider deprecates it and AWS pins it to 1.
  health_check_custom_config {}
}

# One task definition per service, from one map, so that a service added outside
# it is a service with no log group, no ECR repository and no secret grant —
# visibly rather than at apply time.
resource "aws_ecs_task_definition" "svc" {
  for_each = local.services

  family                   = "${var.name}-${each.key}"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = each.value.cpu
  memory                   = each.value.memory
  execution_role_arn       = aws_iam_role.execution.arn

  # No task role, and for the indexer that is a control rather than a default.
  # A task role makes the ECS agent inject AWS_CONTAINER_CREDENTIALS_RELATIVE_URI
  # at RUNTIME and turns 169.254.170.2 into a working credential endpoint —
  # link-local, so no security-group rule touches it — inside the process spec 2
  # calls the untrusted-input boundary. clone.Run appends to os.Environ(), so it
  # would reach git as well, and no assertion over this file's environment block
  # could see it because the variable is not in this file. Nothing in this
  # codebase calls any AWS API, so the role would grant something and buy
  # nothing.
  task_role_arn = null

  dynamic "ephemeral_storage" {
    for_each = each.value.ephemeral_storage_gib == null ? [] : [each.value.ephemeral_storage_gib]
    content {
      # 20 GiB comes free and 21 is the smallest value the field can be SET to,
      # so leaving it unset on the gateway is not the same as setting it to 20.
      size_in_gib = ephemeral_storage.value
    }
  }

  dynamic "volume" {
    for_each = each.value.writable
    content {
      name = replace(trimprefix(volume.value, "/"), "/", "-")
    }
  }

  # From local.container and not written inline, so that the plan-visible
  # output in outputs.tf is the SAME object rather than a copy of it. That
  # matters because secrets[].valueFrom is a Secrets Manager ARN — which carries
  # the account id and a random suffix AWS assigns, so it is unknown at plan
  # time, and one unknown inside a jsonencode makes this whole string unknown.
  # The output replaces exactly that one field and nothing else.
  container_definitions = jsonencode([local.container[each.key]])
}

resource "aws_ecs_service" "svc" {
  for_each = local.services

  name            = each.key
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.svc[each.key].arn
  desired_count   = var.desired_count
  launch_type     = "FARGATE"

  network_configuration {
    subnets = aws_subnet.public[*].id
    # Public addresses and no NAT gateway: $32.85/month before data processing
    # is most of this stack's compute bill. The tasks are protected by their
    # security groups rather than by the absence of a route, and for the indexer
    # that is close to right — its only ingress is a scrape from the scraper.
    assign_public_ip = true
    security_groups  = [each.value.security_group]
  }

  service_registries {
    registry_arn = aws_service_discovery_service.svc[each.key].arn
  }

  # Without this a bad image is a crash-looping service and a green pipeline.
  # It cannot save the FIRST deployment: with no prior COMPLETED deployment
  # there is nothing to roll back to, so a failing first deploy stalls rather
  # than reverting — which is the second reason deploy.yml waits on the embedder
  # before the two services that boot-or-die on it.
  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

  dynamic "load_balancer" {
    for_each = each.value.target_group_arn == null ? [] : [each.value.target_group_arn]
    content {
      target_group_arn = load_balancer.value
      container_name   = each.key
      container_port   = each.value.port
    }
  }
}
