output "repository_urls" {
  description = "Where to push each service's image."
  value       = { for name, repo in aws_ecr_repository.service : name => repo.repository_url }
}

output "log_groups" {
  description = "CloudWatch log group for each service."
  value       = { for name, lg in aws_cloudwatch_log_group.service : name => lg.name }
}

output "ci_push_policy_arn" {
  description = "Attach this to the role or user your CI pipeline runs as."
  value       = aws_iam_policy.ci_push.arn
}
