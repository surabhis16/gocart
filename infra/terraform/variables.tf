variable "project" {
  description = "Name prefix for everything this configuration creates."
  type        = string
  default     = "gocart"
}

variable "region" {
  description = "AWS region."
  type        = string
  default     = "ap-south-1" # Mumbai
}

variable "offline" {
  description = <<-EOT
    true  : `terraform plan` works with no AWS account and no network access to
            AWS. The provider is given throw-away credentials and told not to
            look anything up. Use this for development and in CI. Never apply.
    false : use real credentials from your environment (AWS_PROFILE, SSO, ...).
  EOT
  type        = bool
  default     = true
}

variable "log_retention_days" {
  description = "How long CloudWatch keeps each service's logs."
  type        = number
  default     = 14

  validation {
    condition     = contains([1, 3, 5, 7, 14, 30, 60, 90, 180, 365], var.log_retention_days)
    error_message = "CloudWatch only accepts specific retention periods: 1, 3, 5, 7, 14, 30, 60, 90, 180 or 365 days."
  }
}

variable "images_to_keep" {
  description = "How many images each ECR repository keeps before expiring the oldest."
  type        = number
  default     = 10

  validation {
    condition     = var.images_to_keep >= 1
    error_message = "Keep at least one image."
  }
}
