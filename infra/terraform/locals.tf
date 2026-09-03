locals {
  # PORT is the gateway's and PROBE_PORT is the indexer's, and they are
  # different on purpose: the two run in one network, and a copy-pasted task
  # definition sharing a name would serve the wrong process on the right port.
  gateway_port = 8080
  probe_port   = 9090
  console_port = 80

  # The images this stack pulls. A map rather than a list because every ECR
  # repository, log group, task definition and IAM grant iterates it, and a
  # service added outside the map is a service whose logs nothing can write.
  images = merge(
    {
      gateway = {}
      indexer = {}
      ollama  = {}
    },
    var.console_enabled ? { console = {} } : {},
  )

  # Built from variables and NOT from aws_ecr_repository.svc[k].repository_url,
  # and the reason is the same one that made availability zones a variable: a
  # repository URL contains the account id, skip_requesting_account_id leaves
  # that empty, so the attribute is unknown at plan time — and one unknown field
  # inside a jsonencode makes the WHOLE container_definitions string unknown.
  # Measured. With it derived from the resource, planned_values carries no
  # environment block, no health check, no user, no readonlyRootFilesystem and
  # no secrets, and every assertion in infra/policy about what is inside a
  # container becomes impossible.
  #
  # The repositories are still created here and the execution role still grants
  # on their ARNs; what changes is that the operator names the registry, which
  # deploy.yml has to hand over anyway because it is the thing that just pushed.
  image = {
    for k, _ in local.images : k => "${var.image_registry}/${k}:${var.image_tag}"
  }

  # The database name and the DSN's path. Here rather than inline so Task 6's
  # secret and the RDS instance cannot disagree about it.
  db_name = "codetrail"

  # The indexer's task-definition environment is an allowlist and is asserted
  # as a WHOLE SET, exactly as symbols.EnvKeys is: an assertion that one
  # dangerous name is absent passes when a different dangerous name is added.
  #
  # TYPECHECK_GOPROXY is absent on purpose, so graph.go's default of `off`
  # applies — set to anything else it turns a stranger's require line into an
  # outbound fetch to a host of their choosing, the SSRF §4 exists to refuse
  # reached by a road the host allowlist cannot see.
  #
  # No HTTPS_PROXY, no GIT_*, no GO*. clone.Run appends to os.Environ(), so a
  # proxy variable set here reaches git even though Policy.Env closes it for the
  # compiler: one file feeds two subprocesses under two different disciplines,
  # and only one of them is closed by omission.
  indexer_environment = {
    ALLOWED_HOSTS        = join(",", var.allowed_hosts)
    SCRATCH_DIR          = "/scratch"
    HOME                 = "/scratch"
    PROBE_PORT           = local.probe_port
    EMBED_PROVIDER       = var.embed_provider
    EMBED_MODEL          = var.embed_model
    OLLAMA_URL           = local.ollama_url
    TYPECHECK            = "true"
    MAX_REPO_BYTES       = var.max_repo_bytes
    MAX_REPO_FILES       = var.max_repo_files
    JOB_DEADLINE_SECONDS = var.job_deadline_seconds
    KEEP_REPOS           = var.keep_repos
  }

  gateway_environment = {
    ALLOWED_HOSTS  = join(",", var.allowed_hosts)
    PORT           = local.gateway_port
    EMBED_PROVIDER = var.embed_provider
    EMBED_MODEL    = var.embed_model
    OLLAMA_URL     = local.ollama_url
  }

  # NOTE: the DSN itself is written out in secrets.tf rather than here — see the
  # comment there. This block records what the delivery is and is not.
  #
  # The DSN is delivered through Secrets Manager. The
  # application needs no change and must not get one: config.Get("DATABASE_URL")
  # reads an environment variable, and Secrets Manager is the delivery and not
  # the interface — making the app call Secrets Manager would put an AWS SDK
  # inside a binary whose configuration contract is the environment.
  #
  # sslmode and sslrootcert come from variables rather than being written here,
  # because this string interpolates the database endpoint and is therefore
  # unknown at plan time: the policy suite reads the variables and asserts that
  # this expression references them.

  # The names, which is what the secret resources and the IAM grant iterate.
  secret_names = toset(["database_url"])

  # Never in `environment`: a task definition's environment block is readable by
  # anyone with ecs:DescribeTaskDefinition and this string carries the database
  # password.
  app_secrets = {
    DATABASE_URL = aws_secretsmanager_secret.app["database_url"].arn
  }

  # One map every task definition, service, log group and IAM grant iterates.
  services = merge(
    {
      gateway = {
        cpu                   = 256
        memory                = 512
        port                  = local.gateway_port
        probe                 = "/gateway"
        security_group        = aws_security_group.gateway.id
        target_group_arn      = aws_lb_target_group.gateway.arn
        ephemeral_storage_gib = null
        writable              = ["/tmp"]
        environment           = local.gateway_environment
        secrets               = local.app_secrets
      }
      indexer = {
        cpu    = 1024
        memory = 2048
        # No port mapping the load balancer could ever use: the probe port is
        # reachable from the scraper's security group and from nothing else.
        port           = local.probe_port
        probe          = "/indexer"
        security_group = aws_security_group.indexer.id
        # Never a target group. Asserted, because adding one costs nothing at
        # apply time and publishes the untrusted-input component.
        target_group_arn = null
        # 256 MiB per clone plus the go caches plus headroom for concurrent
        # leases and a checkout that expands.
        ephemeral_storage_gib = 40
        writable              = ["/scratch", "/tmp"]
        environment           = local.indexer_environment
        secrets               = local.app_secrets
      }
    },
    {
      # The embedder both binaries boot-or-die on. embed.FromEnv makes one real
      # round trip at boot with a 30-second deadline, so the model is baked into
      # the image rather than pulled at start — a 274 MB download would race
      # that probe and lose.
      ollama = {
        cpu                   = 2048
        memory                = 4096
        port                  = 11434
        probe                 = null
        security_group        = aws_security_group.ollama.id
        target_group_arn      = null
        ephemeral_storage_gib = null
        writable              = ["/tmp", "/.ollama"]
        environment           = { OLLAMA_HOST = "0.0.0.0:11434" }
        secrets               = {}
      }
    },
    var.console_enabled ? {
      console = {
        cpu                   = 256
        memory                = 512
        port                  = local.console_port
        probe                 = null
        security_group        = aws_security_group.gateway.id
        target_group_arn      = aws_lb_target_group.console[0].arn
        ephemeral_storage_gib = null
        writable              = ["/tmp", "/var/cache/nginx", "/var/run"]
        environment           = {}
        secrets               = {}
      }
    } : {},
  )

  ollama_url = "http://ollama.${var.name}.local:11434"
  # One container definition per service, and the single source both the task
  # definition and outputs.container_shape are built from.
  container = {
    for k, v in local.services : k => {
      name      = k
      image     = local.image[k]
      essential = true

      portMappings = v.port == null ? [] : [{
        containerPort = v.port
        protocol      = "tcp"
      }]

      readonlyRootFilesystem = true
      user                   = "65532:65532"

      linuxParameters = {
        capabilities = { drop = ["ALL"] }
        # The indexer forks git and go; with the application as pid 1 an
        # unreaped child is a zombie per job.
        initProcessEnabled = true
      }

      mountPoints = [
        for p in v.writable : {
          sourceVolume  = replace(trimprefix(p, "/"), "/", "-")
          containerPath = p
          readOnly      = false
        }
      ]

      environment = [
        for n in sort(keys(v.environment)) : {
          name  = n
          value = tostring(v.environment[n])
        }
      ]

      secrets = [
        for n in sort(keys(v.secrets)) : {
          name      = n
          valueFrom = v.secrets[n]
        }
      ]

      # Liveness, not readiness: health.go answers /health from the process and
      # /ready from Postgres, and a container check on the latter turns one
      # shared-datastore outage into a rolling restart of every task — killing
      # the process that could still serve /metrics and say why. The image has
      # no shell, so the request is the binary's own -probe flag.
      healthCheck = v.probe == null ? null : {
        command     = ["CMD", v.probe, "-probe"]
        interval    = 30
        timeout     = 5
        retries     = 3
        startPeriod = 30
      }

      logConfiguration = {
        logDriver = "awslogs"
        options = {
          awslogs-group         = "/ecs/${var.name}/${k}"
          awslogs-region        = var.region
          awslogs-stream-prefix = k
        }
      }
    }
  }
}
