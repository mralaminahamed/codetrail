terraform {
  required_version = ">= 1.6"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
  }

  # Partial configuration, supplied at init from backend.hcl. The bucket is not
  # created by this stack — it has to exist before init — which is exactly why
  # its requirements are written down in backend.hcl.example and the README
  # rather than left to whoever runs the command. State holds the database
  # password in cleartext.
  backend "s3" {}
}

# The four skip_* flags suppress configure-time validation calls and nothing
# else: apply is unaffected. Unconditional rather than behind a variable,
# because a plan that only validates offline when somebody remembers to set a
# flag is a plan nobody validates offline — and every assertion in infra/policy
# reads a plan produced this way, on every pull request, with no account.
#
# They do not make the provider stop *looking* for a credential chain. Measured:
# with no AWS_ACCESS_KEY_ID at all, plan fails with "failed to refresh cached
# credentials, no EC2 IMDS role found". The dummy values CI exports are
# load-bearing and are not decoration.
provider "aws" {
  region                      = var.region
  skip_credentials_validation = true
  skip_requesting_account_id  = true
  skip_region_validation      = true
  skip_metadata_api_check     = true

  default_tags {
    tags = {
      Project = var.name
    }
  }
}
