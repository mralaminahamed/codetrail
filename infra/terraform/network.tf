resource "aws_vpc" "main" {
  cidr_block = var.vpc_cidr
  # Both, because RDS hands out a DNS name and the DSN uses it; without
  # hostnames the endpoint resolves to nothing from inside the VPC.
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = { Name = var.name }
}

resource "aws_internet_gateway" "main" {
  vpc_id = aws_vpc.main.id
  tags   = { Name = var.name }
}

# Public subnets carry the ALB and the tasks. There is no NAT gateway: at
# $0.045/hr it is $32.85/month before data processing, on a stack whose whole
# compute is under $130, so the tasks get public addresses and are protected by
# their security groups rather than by the absence of a route. Stated as the
# trade it is.
resource "aws_subnet" "public" {
  count             = length(var.azs)
  vpc_id            = aws_vpc.main.id
  availability_zone = var.azs[count.index]
  cidr_block        = cidrsubnet(var.vpc_cidr, 8, count.index)

  tags = { Name = "${var.name}-public-${count.index}" }
}

# Private subnets carry the database and nothing else. Offset by 100 so the two
# ranges cannot collide as either grows.
resource "aws_subnet" "private" {
  count             = length(var.azs)
  vpc_id            = aws_vpc.main.id
  availability_zone = var.azs[count.index]
  cidr_block        = cidrsubnet(var.vpc_cidr, 8, count.index + 100)

  tags = { Name = "${var.name}-private-${count.index}" }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.main.id
  }

  tags = { Name = "${var.name}-public" }
}

resource "aws_route_table_association" "public" {
  count          = length(aws_subnet.public)
  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

# The private subnets keep the VPC's default route table, which has no route to
# the internet gateway. That is what makes them private, and it is why the
# database needs no rule of its own to be unreachable from outside.

resource "aws_security_group" "alb" {
  name        = "${var.name}-alb"
  description = "Load balancer: the only thing in this stack that takes traffic from outside the VPC."
  vpc_id      = aws_vpc.main.id

  ingress {
    description = "HTTP from alb_allowed_cidrs"
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = var.alb_allowed_cidrs
  }

  ingress {
    description = "HTTPS from alb_allowed_cidrs"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = var.alb_allowed_cidrs
  }

  # To the VPC only: a load balancer's egress is to its targets, and this stack
  # has no target outside its own network.
  egress {
    description = "To the targets in this VPC"
    from_port   = 0
    to_port     = 65535
    protocol    = "tcp"
    cidr_blocks = [var.vpc_cidr]
  }

  tags = { Name = "${var.name}-alb" }
}
