output "vpc_id" {
  value = aws_vpc.main.id
}

output "public_subnet_ids" {
  value = aws_subnet.public[*].id
}

output "private_subnet_ids" {
  value = aws_subnet.private[*].id
}

# What var.image_registry has to be set to on the next apply. An output rather
# than something derived inside the stack, because the derivation is what would
# make container_definitions unknown at plan time.
output "ecr_registry" {
  value = length(aws_ecr_repository.svc) > 0 ? replace(values(aws_ecr_repository.svc)[0].repository_url, "//[^/]+$/", "") : ""
}

output "alb_dns_name" {
  value = aws_lb.main.dns_name
}

# The container definitions as the plan can see them.
#
# secrets[].valueFrom is a Secrets Manager ARN, which carries the account id and
# a random suffix AWS assigns at creation, so it is unknown at plan time — and
# one unknown inside a jsonencode makes container_definitions wholly unknown,
# leaving the environment allowlist, the user, the read-only root filesystem,
# the dropped capabilities and the health checks with nothing to assert over.
# AWS requires the full ARN there: the name is not accepted, so this cannot be
# built from a variable the way the image reference is.
#
# This is local.container with that ONE field replaced by the secret's name.
# Everything else is the same object the task definition encodes, so nothing
# here can drift from what is deployed.
output "container_shape" {
  value = {
    for k, c in local.container : k => merge(c, {
      secrets = [for s in c.secrets : s.name]
    })
  }
}

output "database_endpoint" {
  value = aws_db_instance.main.address
}

# The DSN itself is deliberately NOT an output. It carries the password, and an
# output is printed by `terraform output`, stored in state and shown in a plan.
