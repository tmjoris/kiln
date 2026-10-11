output "instance_group" {
  description = "Managed instance group of sandbox hosts."
  value       = google_compute_region_instance_group_manager.hosts.instance_group
}

output "artifacts_bucket" {
  value = google_storage_bucket.artifacts.name
}

output "list_hosts" {
  description = "Command to list host IPs for clients (port 7070)."
  value       = "gcloud compute instances list --project ${var.project_id} --filter='name~^${var.name}-host' --format='value(networkInterfaces[0].networkIP)'"
}
