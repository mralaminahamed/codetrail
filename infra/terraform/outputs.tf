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
