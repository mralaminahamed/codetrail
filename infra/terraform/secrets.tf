# One map, and everything that touches a secret iterates it: the versions below
# and the execution role's grant in iam.tf. The sibling project's
# registry-credentials bug was exactly a secret created outside the map the IAM
# grant read.
resource "aws_secretsmanager_secret" "app" {
  for_each = local.secret_names

  name                    = "${var.name}/${each.key}"
  recovery_window_in_days = var.secret_recovery_window_days
}

# Written out per secret rather than for_each over the map, and the reason is
# what can be asserted: with `secret_string = each.value`, the configuration
# section records the expression as `each.value` and the DSN's shape — that it
# carries sslmode and sslrootcert at all — becomes invisible to the plan. The
# IAM grant still iterates the map above, which is where the sibling project's
# bug actually was.
resource "aws_secretsmanager_secret_version" "database_url" {
  secret_id = aws_secretsmanager_secret.app["database_url"].id
  # Written out here and not read from a local, because the plan's configuration
  # section does not expand a local: `secret_string = local.database_url`
  # records one reference and the DSN's shape — that it carries sslmode and
  # sslrootcert at all — becomes invisible to every assertion. Measured.
  secret_string = "postgres://codetrail:${var.db_password}@${aws_db_instance.main.address}:5432/${local.db_name}?sslmode=${var.db_sslmode}&sslrootcert=${var.db_ssl_root_cert}"

  # At plan time, naming the secret — rather than at apply with AWS's
  # "SecretString must not be empty", or at runtime with a task that crash-loops
  # on a DSN it cannot parse.
  lifecycle {
    precondition {
      # sslrootcert is the one with no variable validation behind it, and it
      # is the one whose absence turns verify-full into a boot failure about a
      # certificate rather than about a setting.
      condition     = var.db_ssl_root_cert != "" && var.db_sslmode != ""
      error_message = "The database_url secret would be assembled from an empty password or sslmode; the task reading it would crash-loop on a value nothing validated."
    }
  }
}
