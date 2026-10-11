# Build outputs are uploaded content-addressed: a new kilnd or image gets a
# new object name, which changes the instance template and rolls the pool.

resource "google_storage_bucket" "artifacts" {
  name                        = "${var.project_id}-${var.name}-artifacts"
  location                    = var.region
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  force_destroy               = true
}

locals {
  artifacts = {
    kilnd         = "${var.bin_dir}/kilnd"
    kiln          = "${var.bin_dir}/kiln"
    vmlinux       = "${var.build_dir}/kernel/vmlinux"
    "rootfs.ext4" = "${var.build_dir}/images/python/rootfs.ext4"
    "rootfs.tar"  = "${var.build_dir}/images/python/rootfs.tar"
  }
}

resource "google_storage_bucket_object" "artifact" {
  for_each = local.artifacts
  bucket   = google_storage_bucket.artifacts.name
  name     = "${each.key}/${filemd5(each.value)}"
  source   = each.value
}

resource "google_service_account" "host" {
  account_id   = "${var.name}-host"
  display_name = "Kiln sandbox host"
}

resource "google_storage_bucket_iam_member" "host_reads_artifacts" {
  bucket = google_storage_bucket.artifacts.name
  role   = "roles/storage.objectViewer"
  member = "serviceAccount:${google_service_account.host.email}"
}

resource "google_project_iam_member" "host_logging" {
  for_each = toset(["roles/logging.logWriter", "roles/monitoring.metricWriter"])
  project  = var.project_id
  role     = each.value
  member   = "serviceAccount:${google_service_account.host.email}"
}
