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

# No default, for the reason alb_allowed_cidrs has none: it has to be an answer.
# The images this stack pulls come from the ECR repositories it creates, whose
# host carries the account id — and the account id is exactly what
# skip_requesting_account_id leaves empty so that plan needs no credentials.
# `terraform output ecr_registry` after the first apply prints it, and deploy.yml
# passes it, because the workflow is what just pushed there.
#
# ECR and not GHCR: this repository is private, GHCR packages inherit repository
# visibility, and a private GHCR image fails a Fargate pull with
# CannotPullContainerError while the circuit breaker rolls the apply back. ECR
# is pulled by the execution role through IAM, so there is no registry
# credential at all.
variable "image_registry" {
  type        = string
  description = "Registry host and namespace the images are pulled from, e.g. 123456789012.dkr.ecr.us-east-1.amazonaws.com/codetrail."

  validation {
    condition     = length(var.image_registry) > 0
    error_message = "image_registry must name the registry the images were pushed to; `terraform output ecr_registry` prints it after the first apply."
  }
}

variable "image_tag" {
  type        = string
  default     = "latest"
  description = "Image tag to deploy. The commit SHA is the only tag that cannot move under a running service; latest is the default only so that a first apply has something to pull, and a deploy that names it is a deploy nobody can reproduce."
}

variable "desired_count" {
  type        = number
  default     = 1
  description = "Tasks per service. 0 parks the stack: the ALB and the database stay, so the URL and the corpus survive, and eviction and the job-history sweep stop with the indexer that runs them."

  validation {
    condition     = var.desired_count >= 0
    error_message = "desired_count must not be negative."
  }
}

variable "console_enabled" {
  type        = bool
  default     = false
  description = "Serve the console bundle as the load balancer's default action. False until P5 ships an image, so that P8 can land without a service that has nothing to run."
}

variable "observability_enabled" {
  type        = bool
  default     = false
  description = "Run Prometheus in the VPC. The same rule file evaluates locally under compose for nothing, and this task has no persistence and no Alertmanager."
}

variable "certificate_arn" {
  type        = string
  default     = ""
  description = "ACM certificate for an HTTPS listener. Empty by default because this stack owns no domain: the load balancer's security group still admits 443, and with no certificate nothing listens on it."
}

# The allowlist §4 calls the SSRF control, written explicitly into the task
# definition rather than left to the binary's default. The deployment's job here
# is not to weaken it: a widening is then a reviewed diff and a red build rather
# than a forgotten environment variable.
variable "allowed_hosts" {
  type        = list(string)
  default     = ["github.com", "codeberg.org"]
  description = "Exact hosts the indexer may clone from. A subset of admit.DefaultHosts; the policy suite fails on anything outside it."
}

variable "max_repo_bytes" {
  type        = number
  default     = 268435456
  description = "Clone byte cap, 256 MiB. Bounds one job and not the arrival rate."
}

variable "max_repo_files" {
  type        = number
  default     = 20000
  description = "Walk file-count cap."
}

variable "job_deadline_seconds" {
  type        = number
  default     = 600
  description = "Wall-clock budget every stage of one job shares."
}

variable "keep_repos" {
  type        = number
  default     = 50
  description = "Repositories the corpus may hold before LRU eviction removes the least recently queried."

  validation {
    condition     = var.keep_repos > 0
    error_message = "keep_repos must be positive: the indexer reads 0 as keep nothing and would delete every repository."
  }
}

variable "log_retention_days" {
  type        = number
  default     = 14
  description = "CloudWatch Logs retention."
}

# Restricted to ollama, and the refusal is the point: embed.FromEnv accepts
# `fake` too, and a fake embedder serves meaningless retrieval behind a wall of
# 200s — nothing about a response would look wrong. It is banned here and used
# deliberately under compose and in CI, where nothing depends on embedding
# quality.
variable "embed_provider" {
  type        = string
  default     = "ollama"
  description = "Embedder for both binaries. Only ollama: a fake embedder is meaningless retrieval that every status code reports as healthy."

  validation {
    condition     = var.embed_provider == "ollama"
    error_message = "embed_provider must be ollama. `fake` would serve meaningless retrieval behind a wall of 200s, which is the silent downgrade this project exists to refuse."
  }
}

variable "embed_model" {
  type        = string
  default     = "nomic-embed-text"
  description = "Embedding model. Must produce 768 dimensions: store.EmbeddingDim is a constant and store.CheckDim refuses any other width at boot."
}

# Supplied by the deploy workflow from a GitHub Actions secret, and NOT
# random_password: that would put the generated value in state anyway while
# adding a resource whose whole purpose is to be unpredictable, in a stack whose
# plan output this phase asserts over.
#
# manage_master_user_password = true is the better design and is deliberately
# not chosen here: reading the generated secret back to assemble the DSN needs a
# data source, a data source is an API call, and one API call turns terraform
# plan — and this phase's whole policy suite — into something that needs an
# account. It is the first thing to change once there is one.
variable "db_password" {
  type        = string
  sensitive   = true
  description = "RDS master password. URI-safe characters only: it is interpolated into the DSN."

  validation {
    condition     = length(var.db_password) >= 16
    error_message = "db_password must be at least 16 characters."
  }

  validation {
    condition     = can(regex("^[A-Za-z0-9._~-]+$", var.db_password))
    error_message = "db_password must be URI-safe (A-Z a-z 0-9 . _ ~ -): it is interpolated into DATABASE_URL, and a reserved character would silently truncate the DSN."
  }
}

# Pinned to a 17 minor. auto_minor_version_upgrade stays on, because the
# alternative is a database that never gets a security fix — and what catches an
# upgrade moving pgvector underneath a running deployment is the smoke test's
# version assertion, not this pin.
variable "db_engine_version" {
  type        = string
  default     = "17.7"
  description = "PostgreSQL minor. pgvector must be >= 0.5.0, which is the HNSW floor migrations/0002_span_ann.sql builds against — not 0.8.0: hnsw.iterative_scan does not exist below 0.8.0, and the code assumes no compensation anyway."
}

variable "db_instance_class" {
  type        = string
  default     = "db.t4g.micro"
  description = "RDS instance class."
}

variable "db_allocated_storage" {
  type        = number
  default     = 20
  description = "GiB of gp3 storage."
}

# verify-full and not require. `require` encrypts and does NOT authenticate the
# server, so it stops a passive listener and not a man in the middle, and
# nothing in any log distinguishes the two. A variable rather than a literal
# because the DSN it is interpolated into carries the database endpoint, which
# is unknown at plan time — one unknown makes the whole string unknown, and the
# policy suite would then have nothing to assert over. This is what makes C13 a
# plan-time failure rather than something found in production.
variable "db_sslmode" {
  type        = string
  default     = "verify-full"
  description = "libpq sslmode for DATABASE_URL. verify-full: anything weaker encrypts without authenticating the server."
}

variable "db_ssl_root_cert" {
  type        = string
  default     = "/etc/ssl/certs/rds-global-bundle.pem"
  description = "Path to the Amazon RDS trust store inside both images. Every certificate in that bundle is a self-signed RDS root and none chains to a publicly trusted CA, so verify-full against the image's default store cannot validate the server."
}

variable "secret_recovery_window_days" {
  type        = number
  default     = 0
  description = "Secrets Manager recovery window. 0 trades recoverability for repeatability, which is the right trade for a stack whose cheapest operating mode is terraform destroy — and the wrong one once it holds a key in use."
}
