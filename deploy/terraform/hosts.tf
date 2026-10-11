resource "google_compute_instance_template" "host" {
  name_prefix  = "${var.name}-host-"
  machine_type = var.machine_type
  tags         = ["${var.name}-host"]

  # Firecracker needs /dev/kvm inside the VM.
  advanced_machine_features {
    enable_nested_virtualization = true
    threads_per_core             = 1 # no SMT siblings shared across tenants
  }
  min_cpu_platform = "Intel Cascade Lake"

  disk {
    source_image = "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64"
    disk_type    = "pd-ssd"
    disk_size_gb = var.boot_disk_gb
    boot         = true
    auto_delete  = true
  }

  network_interface {
    subnetwork = google_compute_subnetwork.hosts.id
  }

  service_account {
    email  = google_service_account.host.email
    scopes = ["cloud-platform"]
  }

  shielded_instance_config {
    enable_secure_boot          = true
    enable_integrity_monitoring = true
  }

  metadata = {
    enable-oslogin = "TRUE"
    startup-script = templatefile("${path.module}/startup.sh.tftpl", {
      bucket              = google_storage_bucket.artifacts.name
      objects             = { for k, o in google_storage_bucket_object.artifact : k => o.name }
      firecracker_version = var.firecracker_version
      firecracker_sha256  = var.firecracker_sha256
      gvisor_release      = var.gvisor_release
      gvisor_sha256       = var.gvisor_sha256
      kilnd_flags         = var.kilnd_flags
    })
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "google_compute_health_check" "kilnd" {
  name                = "${var.name}-kilnd"
  check_interval_sec  = 10
  timeout_sec         = 5
  healthy_threshold   = 2
  unhealthy_threshold = 3
  grpc_health_check {
    port = 7070
  }
}

resource "google_compute_region_instance_group_manager" "hosts" {
  name               = "${var.name}-hosts"
  region             = var.region
  base_instance_name = "${var.name}-host"
  target_size        = var.host_count

  version {
    instance_template = google_compute_instance_template.host.id
  }

  auto_healing_policies {
    health_check = google_compute_health_check.kilnd.id
    # Template boot + snapshot build + pool fill.
    initial_delay_sec = 300
  }

  # Replacing a host destroys its sandboxes: roll one at a time, surge first.
  update_policy {
    type                  = "PROACTIVE"
    minimal_action        = "REPLACE"
    max_surge_fixed       = 3
    max_unavailable_fixed = 0
  }
}
