resource "aws_lb" "main" {
  name               = var.name
  load_balancer_type = "application"
  security_groups    = [aws_security_group.alb.id]
  subnets            = aws_subnet.public[*].id

  # Nothing here terminates TLS between the load balancer and a task, so a
  # header a client sent must not be trusted as one the load balancer set.
  drop_invalid_header_fields = true
}

# The health check is /ready and NOT /health, and the two fail in opposite
# directions. health.go answers /health with an unconditional 200, so a load
# balancer probing it keeps a gateway in rotation that has lost Postgres and is
# failing every query behind a healthy target. The container health check in
# ecs.tf is the mirror of this and probes /health, because a container check on
# readiness turns one shared-datastore outage into a rolling restart of
# everything.
resource "aws_lb_target_group" "gateway" {
  name        = "${var.name}-gateway"
  port        = local.gateway_port
  protocol    = "HTTP"
  target_type = "ip"
  vpc_id      = aws_vpc.main.id

  health_check {
    path                = "/ready"
    matcher             = "200"
    interval            = 30
    timeout             = 5
    healthy_threshold   = 2
    unhealthy_threshold = 3
  }

  deregistration_delay = 30
}

resource "aws_lb_target_group" "console" {
  count = var.console_enabled ? 1 : 0

  name        = "${var.name}-console"
  port        = local.console_port
  protocol    = "HTTP"
  target_type = "ip"
  vpc_id      = aws_vpc.main.id

  health_check {
    path    = "/"
    matcher = "200"
  }

  deregistration_delay = 30
}

# There is deliberately no target group for the indexer. It is spec 2's
# untrusted-input component and serves nothing anyone outside the VPC should
# reach; adding one costs nothing at apply time and publishes it, which is why
# the absence is asserted rather than left to be noticed.

# One load balancer and one hostname in front of both processes, because P5's
# console has no CORS to fall back on and no configurable API base: its API base
# is the relative prefix /api, so a second origin is a console that cannot talk
# to anything.
resource "aws_lb_listener" "http" {
  load_balancer_arn = aws_lb.main.arn
  port              = 80
  protocol          = "HTTP"

  # The default action is a 404 and not the gateway. That is what keeps
  # /health, /ready and /metrics unpublished: with the gateway as the default,
  # every path the process serves at the root would be reachable from the
  # internet, and health.go mounts all three there.
  dynamic "default_action" {
    for_each = var.console_enabled ? [1] : []
    content {
      type             = "forward"
      target_group_arn = aws_lb_target_group.console[0].arn
    }
  }

  dynamic "default_action" {
    for_each = var.console_enabled ? [] : [1]
    content {
      type = "fixed-response"
      fixed_response {
        content_type = "text/plain"
        message_body = "not found"
        status_code  = "404"
      }
    }
  }
}

# The only path published to the gateway. Everything the process serves outside
# /api — the probes and the metrics — has no rule and is reachable only by the
# target group's health check and, for /metrics, by the scraper inside the VPC.
resource "aws_lb_listener_rule" "api" {
  listener_arn = aws_lb_listener.http.arn
  priority     = 100

  action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.gateway.arn
  }

  condition {
    path_pattern {
      values = ["/api/*"]
    }
  }
}

# HTTPS only when somebody supplies a certificate. This stack owns no domain, so
# with certificate_arn empty the load balancer's security group admits 443 and
# nothing listens on it.
resource "aws_lb_listener" "https" {
  count = var.certificate_arn != "" ? 1 : 0

  load_balancer_arn = aws_lb.main.arn
  port              = 443
  protocol          = "HTTPS"
  ssl_policy        = "ELBSecurityPolicy-TLS13-1-2-2021-06"
  certificate_arn   = var.certificate_arn

  default_action {
    type             = "forward"
    target_group_arn = var.console_enabled ? aws_lb_target_group.console[0].arn : aws_lb_target_group.gateway.arn
  }
}
