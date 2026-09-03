# alb_allowed_cidrs is first in the file because it is the decision with the
# largest consequence in the stack, and it has no default so that it has to be
# an answer.
#
# What sits behind this load balancer is POST /api/repos, which has no
# authentication and no rate limit and which runs `git clone` against a URL a
# stranger supplied, on your bill (spec:145-149). The per-job caps bound one
# job; nothing bounds the arrival rate. ["0.0.0.0/0"] is a legitimate answer for
# a public demo — it just has to be one somebody wrote down.
variable "alb_allowed_cidrs" {
  type        = list(string)
  description = "CIDRs allowed to reach the load balancer. No default: this publishes an unauthenticated endpoint that clones stranger-supplied repositories."

  validation {
    condition     = length(var.alb_allowed_cidrs) > 0
    error_message = "alb_allowed_cidrs must name at least one CIDR; an empty list would publish nothing and is more likely a mistake than an intent."
  }

  validation {
    condition     = alltrue([for c in var.alb_allowed_cidrs : can(cidrhost(c, 0))])
    error_message = "Every entry of alb_allowed_cidrs must be a CIDR block, e.g. 203.0.113.0/24."
  }
}

variable "name" {
  type        = string
  default     = "codetrail"
  description = "Name prefix for every resource in the stack."
}

variable "region" {
  type        = string
  default     = "us-east-1"
  description = "AWS region. Changing it means changing azs too: they are a variable and not a data source, so nothing derives them for you."
}

# A variable and not data "aws_availability_zones", and this is the single
# decision that keeps `terraform plan` runnable with no AWS account: that data
# source is the one thing in a stack this shape that forces a real API call at
# plan time, and with it the whole of infra/policy becomes account-required.
# The cost is that an operator changing region must set this too.
variable "azs" {
  type        = list(string)
  default     = ["us-east-1a", "us-east-1b"]
  description = "Exactly two availability zones, in var.region. A variable rather than a data source so that plan needs no credentials."

  validation {
    condition     = length(var.azs) == 2
    error_message = "azs must name exactly two zones: the ALB requires two subnets and RDS requires a subnet group spanning two."
  }
}

variable "vpc_cidr" {
  type        = string
  default     = "10.0.0.0/16"
  description = "The stack's VPC CIDR."

  validation {
    condition     = can(cidrhost(var.vpc_cidr, 0))
    error_message = "vpc_cidr must be a CIDR block."
  }
}
