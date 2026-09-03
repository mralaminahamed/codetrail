resource "aws_db_subnet_group" "main" {
  name       = var.name
  subnet_ids = aws_subnet.private[*].id
}

# Single-AZ and skip_final_snapshot, stated as a decision rather than left as a
# default: every row in this database is derived. Repositories are re-indexed
# from their remotes, RepoID = hash(remote, commit) makes re-indexing converge,
# and spec §4 already treats the corpus as evictable — so a standby for a
# rebuildable index is money spent on the wrong risk.
resource "aws_db_instance" "main" {
  identifier     = var.name
  engine         = "postgres"
  engine_version = var.db_engine_version
  instance_class = var.db_instance_class

  allocated_storage = var.db_allocated_storage
  storage_type      = "gp3"
  storage_encrypted = true

  db_name  = local.db_name
  username = "codetrail"
  password = var.db_password

  db_subnet_group_name   = aws_db_subnet_group.main.name
  vpc_security_group_ids = [aws_security_group.data.id]
  # Private subnets plus a security group that admits only the two application
  # groups. This is the third lock and the one an assertion can read.
  publicly_accessible = false

  multi_az                    = false
  auto_minor_version_upgrade  = true
  backup_retention_period     = 1
  skip_final_snapshot         = true
  allow_major_version_upgrade = false

  # CREATE EXTENSION IF NOT EXISTS vector runs in migrations/0001_init.sql under
  # the master user's rds_superuser role. RDS offers pgvector as a managed
  # extension, so nothing here has to install it — but nothing here can prove
  # that either, which is why the smoke test reads the version off the server.
  apply_immediately = true
}
