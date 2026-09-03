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

# The gateway and the indexer have different security groups, and that is the
# resource §2's sentence becomes: "splitting it makes the sandbox a deployment
# boundary rather than a code convention" is only true if the two do not share
# one set of firewall rules.
#
# What actually differs, enumerated rather than gestured at: ingress, whether
# there is a target group, whether there is a task role, and the writable
# surface. What does NOT differ is egress, and that is a real limit rather than
# an oversight — a Fargate task pulls its own image, reads its own secrets and
# ships its own logs through its task ENI, and there is no AWS-managed prefix
# list for ECR, Secrets Manager or CloudWatch Logs to scope 443 with. Interface
# VPC endpoints would earn the stronger claim at about $29/month and are not
# built here.

resource "aws_security_group" "gateway" {
  name        = "${var.name}-gateway"
  description = "Gateway tasks: the public API, reachable only through the load balancer."
  vpc_id      = aws_vpc.main.id

  ingress {
    description     = "The service port, from the load balancer only"
    from_port       = local.gateway_port
    to_port         = local.gateway_port
    protocol        = "tcp"
    security_groups = [aws_security_group.alb.id]
  }

  # Enumerated, never protocol = "-1". A single -1 rule to 0.0.0.0/0 is what
  # every ECS example ships and it opens tcp/22, tcp/9418 and every UDP port.
  #
  # This restricts direction and port and says nothing about which HOSTS are
  # reachable, and it must not be read as if it did: §4 puts the host decision
  # in admit.Policy, in the process, and states that codetrail claims no
  # DNS-rebinding protection. A prefix list built from a forge's published
  # ranges goes stale silently and a TLS-terminating proxy is a second trust
  # boundary; neither is shipped.
  egress {
    description = "Secrets Manager, ECR and CloudWatch Logs, all reached through this task ENI"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  # To the VPC by CIDR and not to the data security group by id, because the
  # database's own ingress names these two groups and the pair would be a
  # dependency cycle. The binding restriction is on that side: this one narrows
  # the port, and aws_security_group.data decides who may connect.
  # The embedder. Both binaries boot-or-die on one real round trip to it
  # (embed.FromEnv), so an egress set without this port is a stack that cannot
  # start — which the plan file's enumerated list left out.
  egress {
    description = "Ollama, inside this VPC"
    from_port   = 11434
    to_port     = 11434
    protocol    = "tcp"
    cidr_blocks = [var.vpc_cidr]
  }

  egress {
    description = "Postgres, inside this VPC"
    from_port   = 5432
    to_port     = 5432
    protocol    = "tcp"
    cidr_blocks = [var.vpc_cidr]
  }

  # Both destinations, because the resolver's address differs between the VPC
  # .2 address and the link-local Route 53 endpoint, and a task that cannot
  # resolve a name fails with something that reads like a network outage.
  egress {
    description = "DNS over UDP"
    from_port   = 53
    to_port     = 53
    protocol    = "udp"
    cidr_blocks = [var.vpc_cidr, "169.254.169.253/32"]
  }

  egress {
    description = "DNS over TCP"
    from_port   = 53
    to_port     = 53
    protocol    = "tcp"
    cidr_blocks = [var.vpc_cidr, "169.254.169.253/32"]
  }

  tags = { Name = "${var.name}-gateway" }
}

resource "aws_security_group" "indexer" {
  name        = "${var.name}-indexer"
  description = "Indexer tasks: spec 2's untrusted-input boundary. Accepts nothing but a scrape."
  vpc_id      = aws_vpc.main.id

  # The only thing that may open a connection to this process is the scraper,
  # and only on the probe port. There is no load balancer in front of it and
  # there is no rule admitting a CIDR.
  ingress {
    description     = "The probe port, from the scraper only"
    from_port       = local.probe_port
    to_port         = local.probe_port
    protocol        = "tcp"
    security_groups = [aws_security_group.observability.id]
  }

  egress {
    description = "git clone over https, plus the image, the secrets and the logs"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  # To the VPC by CIDR and not to the data security group by id, because the
  # database's own ingress names these two groups and the pair would be a
  # dependency cycle. The binding restriction is on that side: this one narrows
  # the port, and aws_security_group.data decides who may connect.
  # The embedder. Both binaries boot-or-die on one real round trip to it
  # (embed.FromEnv), so an egress set without this port is a stack that cannot
  # start — which the plan file's enumerated list left out.
  egress {
    description = "Ollama, inside this VPC"
    from_port   = 11434
    to_port     = 11434
    protocol    = "tcp"
    cidr_blocks = [var.vpc_cidr]
  }

  egress {
    description = "Postgres, inside this VPC"
    from_port   = 5432
    to_port     = 5432
    protocol    = "tcp"
    cidr_blocks = [var.vpc_cidr]
  }

  egress {
    description = "DNS over UDP"
    from_port   = 53
    to_port     = 53
    protocol    = "udp"
    cidr_blocks = [var.vpc_cidr, "169.254.169.253/32"]
  }

  egress {
    description = "DNS over TCP"
    from_port   = 53
    to_port     = 53
    protocol    = "tcp"
    cidr_blocks = [var.vpc_cidr, "169.254.169.253/32"]
  }

  tags = { Name = "${var.name}-indexer" }
}

# Created whether or not Prometheus runs, because the indexer's one ingress rule
# names it: a group that exists and holds no member admits nothing, which is the
# correct behaviour when observability is off.
resource "aws_security_group" "observability" {
  name        = "${var.name}-observability"
  description = "The scraper. The only thing allowed to open a connection to the indexer."
  vpc_id      = aws_vpc.main.id

  egress {
    description = "Scrape both services"
    from_port   = 0
    to_port     = 65535
    protocol    = "tcp"
    cidr_blocks = [var.vpc_cidr]
  }

  tags = { Name = "${var.name}-observability" }
}

resource "aws_security_group" "data" {
  name        = "${var.name}-data"
  description = "Postgres. Reachable from the two application security groups and from nothing else."
  vpc_id      = aws_vpc.main.id

  ingress {
    description     = "Postgres from the gateway"
    from_port       = 5432
    to_port         = 5432
    protocol        = "tcp"
    security_groups = [aws_security_group.gateway.id]
  }

  ingress {
    description     = "Postgres from the indexer"
    from_port       = 5432
    to_port         = 5432
    protocol        = "tcp"
    security_groups = [aws_security_group.indexer.id]
  }

  tags = { Name = "${var.name}-data" }
}

resource "aws_security_group" "ollama" {
  name        = "${var.name}-ollama"
  description = "The embedder. Reachable from the two application security groups and from nothing else."
  vpc_id      = aws_vpc.main.id

  ingress {
    description     = "Embeddings from the gateway"
    from_port       = 11434
    to_port         = 11434
    protocol        = "tcp"
    security_groups = [aws_security_group.gateway.id]
  }

  ingress {
    description     = "Embeddings from the indexer"
    from_port       = 11434
    to_port         = 11434
    protocol        = "tcp"
    security_groups = [aws_security_group.indexer.id]
  }

  egress {
    description = "Its own image, and its own logs"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  egress {
    description = "DNS over UDP"
    from_port   = 53
    to_port     = 53
    protocol    = "udp"
    cidr_blocks = [var.vpc_cidr, "169.254.169.253/32"]
  }

  egress {
    description = "DNS over TCP"
    from_port   = 53
    to_port     = 53
    protocol    = "tcp"
    cidr_blocks = [var.vpc_cidr, "169.254.169.253/32"]
  }

  tags = { Name = "${var.name}-ollama" }
}
