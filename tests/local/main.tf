terraform {
  required_version = "= 1.13.8"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "= 6.41.0"
    }
  }
  backend "s3" {
    bucket                      = "cosmoformer-state"
    key                         = "local/terraform.tfstate"
    region                      = "us-east-1"
    endpoints                   = { s3 = "http://seaweed:8333" }
    use_path_style              = true
    use_lockfile                = true
    skip_credentials_validation = true
    skip_region_validation      = true
    skip_metadata_api_check     = true
    skip_requesting_account_id  = true
  }
}

provider "aws" {
  region = "us-east-1"
  endpoints {
    s3 = "http://seaweed:8333"
  }
  s3_use_path_style           = true
  skip_credentials_validation = true
  skip_region_validation      = true
  skip_metadata_api_check     = true
  skip_requesting_account_id  = true
}

variable "content" {
  type    = string
  default = "hello from local OpenTofu"
}

# The harness owns these buckets. Tofu owns only this disposable object.
resource "aws_s3_object" "example" {
  bucket  = "cosmoformer-resources"
  key     = "example.txt"
  content = var.content
  etag    = md5(var.content)
}

output "object_key" {
  value = aws_s3_object.example.key
}
