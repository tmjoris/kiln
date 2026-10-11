# Hosts get no public IPs. Egress (release downloads) goes through Cloud NAT;
# operators reach them through IAP. Sandboxes themselves have no network at
# all, so NAT only ever serves the host OS.

resource "google_compute_network" "kiln" {
  name                    = var.name
  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "hosts" {
  name                     = "${var.name}-hosts"
  network                  = google_compute_network.kiln.id
  region                   = var.region
  ip_cidr_range            = var.subnet_cidr
  private_ip_google_access = true
}

resource "google_compute_router" "kiln" {
  name    = var.name
  network = google_compute_network.kiln.id
  region  = var.region
}

resource "google_compute_router_nat" "kiln" {
  name                               = var.name
  router                             = google_compute_router.kiln.name
  region                             = var.region
  nat_ip_allocate_option             = "AUTO_ONLY"
  source_subnetwork_ip_ranges_to_nat = "ALL_SUBNETWORKS_ALL_IP_RANGES"
}

resource "google_compute_firewall" "grpc_clients" {
  name          = "${var.name}-grpc"
  network       = google_compute_network.kiln.id
  direction     = "INGRESS"
  source_ranges = var.client_cidrs
  target_tags   = ["${var.name}-host"]
  allow {
    protocol = "tcp"
    ports    = ["7070"]
  }
}

# Google health checkers, for MIG autohealing.
resource "google_compute_firewall" "health_checks" {
  name          = "${var.name}-health"
  network       = google_compute_network.kiln.id
  direction     = "INGRESS"
  source_ranges = ["35.191.0.0/16", "130.211.0.0/22"]
  target_tags   = ["${var.name}-host"]
  allow {
    protocol = "tcp"
    ports    = ["7070"]
  }
}

resource "google_compute_firewall" "iap_ssh" {
  name          = "${var.name}-iap-ssh"
  network       = google_compute_network.kiln.id
  direction     = "INGRESS"
  source_ranges = ["35.235.240.0/20"]
  target_tags   = ["${var.name}-host"]
  allow {
    protocol = "tcp"
    ports    = ["22"]
  }
}
