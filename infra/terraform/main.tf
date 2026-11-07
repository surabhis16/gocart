# What GoCart needs from a cloud, as code.
#
# Each service gets:
#   - a container registry (ECR), because "shipping the container" has to end
#     somewhere;
#   - a log group (CloudWatch), because a service nobody can read the logs of
#     is not operable.
# Plus one IAM policy: the least a CI job needs to push images, and no more.
#
# There is deliberately no VPC, load balancer or cluster here. Those are the
# expensive parts of AWS, and this project runs on a laptop. The point is the
# practice: infrastructure described in files, reviewed, planned and applied by
# a pipeline (see .github/workflows/ci.yml) instead of clicked together.

provider "aws" {
  region = var.region

  # Offline mode: lets `terraform plan` run with no AWS account. Nothing below
  # reads from AWS (no data sources that call the API), so the plan is complete.
  access_key                  = var.offline ? "offline" : null
  secret_key                  = var.offline ? "offline" : null
  skip_credentials_validation = var.offline
  skip_requesting_account_id  = var.offline
  skip_metadata_api_check     = var.offline

  default_tags {
    tags = local.tags
  }
}

locals {
  services = toset(["product-service", "order-service", "payment-service", "cart-service", "web-service"])

  tags = {
    Project   = var.project
    ManagedBy = "terraform"
  }
}

resource "aws_ecr_repository" "service" {
  for_each = local.services

  name = "${var.project}/${each.key}"

  # A tag names one image forever. Re-pushing 0.1.0 with different contents
  # would make "what is running in staging?" unanswerable.
  image_tag_mutability = "IMMUTABLE"

  image_scanning_configuration {
    scan_on_push = true
  }

  encryption_configuration {
    encryption_type = "AES256"
  }
}

resource "aws_ecr_lifecycle_policy" "service" {
  for_each = aws_ecr_repository.service

  repository = each.value.name

  policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "Keep only the newest ${var.images_to_keep} images"
      selection = {
        tagStatus   = "any"
        countType   = "imageCountMoreThan"
        countNumber = var.images_to_keep
      }
      action = { type = "expire" }
    }]
  })
}

resource "aws_cloudwatch_log_group" "service" {
  for_each = local.services

  name              = "/${var.project}/${each.key}"
  retention_in_days = var.log_retention_days
}

data "aws_iam_policy_document" "ci_push" {
  statement {
    sid       = "LogInToEcr"
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"] # this one action does not support resource-level permissions
  }

  statement {
    sid = "PushToGocartRepositoriesOnly"
    actions = [
      "ecr:BatchCheckLayerAvailability",
      "ecr:InitiateLayerUpload",
      "ecr:UploadLayerPart",
      "ecr:CompleteLayerUpload",
      "ecr:PutImage",
    ]
    resources = [for repo in aws_ecr_repository.service : repo.arn]
  }
}

resource "aws_iam_policy" "ci_push" {
  name        = "${var.project}-ci-push"
  description = "Lets CI push GoCart images to ECR, and nothing else."
  policy      = data.aws_iam_policy_document.ci_push.json
}
