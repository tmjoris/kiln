variable "project_id" {
  description = "GCP project to deploy into."
  type        = string
}

variable "region" {
  description = "Region for the host pool. Hosts spread across its zones."
  type        = string
  default     = "us-central1"
}

variable "name" {
  description = "Prefix for every resource."
  type        = string
  default     = "kiln"
}

variable "host_count" {
  description = "Sandbox hosts in the pool. Fixed size: scale-in would destroy live sandboxes."
  type        = number
  default     = 2
}

variable "machine_type" {
  description = "Host shape. Must support nested virtualization (N2/N2D/C3 Intel or AMD)."
  type        = string
  default     = "n2-standard-8"
}

variable "boot_disk_gb" {
  description = "Boot disk size; holds images, snapshots and per-VM dirs."
  type        = number
  default     = 100
}

variable "client_cidrs" {
  description = "CIDRs allowed to call the Kiln gRPC API (e.g. the agent orchestrator's subnet)."
  type        = list(string)
  default     = ["10.0.0.0/8"]
}

variable "subnet_cidr" {
  description = "Primary range of the hosts' subnet."
  type        = string
  default     = "10.42.0.0/20"
}

variable "build_dir" {
  description = "Local build output (make build image) uploaded as host artifacts."
  type        = string
  default     = "../../build"
}

variable "bin_dir" {
  description = "Local directory with linux/amd64 kilnd and kiln binaries."
  type        = string
  default     = "../../bin"
}

variable "firecracker_version" {
  type    = string
  default = "v1.17.0"
}

variable "firecracker_sha256" {
  description = "sha256 of firecracker-<version>-x86_64.tgz (matches upstream's .sha256.txt)."
  type        = string
  default     = "06094a1108ae9e82aa4c23a775aa92758f53f1175d422270d9d6162cb9ade558"
}

variable "gvisor_release" {
  type    = string
  default = "20261005.0"
}

variable "gvisor_sha256" {
  description = "sha256 of gvisor-x86_64.tar.bz2 for gvisor_release."
  type        = string
  default     = "bd4eb2ca0ee74f0fbbb5f5273ba011e79c56a29fbb047393b88b981cd3230ede"
}

variable "kilnd_flags" {
  description = "Extra flags for kilnd (pools, shapes, limits)."
  type        = string
  default     = "-backends firecracker,gvisor -pools firecracker/python=16,gvisor/python=4 -mem-mib 512 -min-mem-available-mib 4096"
}
