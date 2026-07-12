# gastown EC2 worker AMI — structural stub, not production-ready.
#
# Defines the stable base image for the EC2 execution provider
# (docs/design/remote-polecat-execution-ec2.md §3). The AMI carries only
# slow-moving infrastructure: dockerd, amazon-ssm-agent, git, the gt-agent
# system user, and the gt-worker-agent bootstrapper. Version-sensitive
# binaries (gt, bd, proxy-client, gt-worker-agent) are injected at boot over
# SSM and MUST NOT be baked here. No certificates, keys, tokens, or rig
# config in the AMI, ever.

packer {
  required_plugins {
    amazon = {
      source  = "github.com/hashicorp/amazon"
      version = ">= 1.3.0"
    }
  }
}

variable "region" {
  type    = string
  default = "us-east-1"
}

variable "instance_type" {
  type    = string
  default = "t3.large"
}

variable "ami_name_prefix" {
  type    = string
  default = "gt-worker-ec2"
}

source "amazon-ebs" "worker" {
  region        = var.region
  instance_type = var.instance_type
  ami_name      = "${var.ami_name_prefix}-{{timestamp}}"
  ssh_username  = "ec2-user"

  # Base distro: Amazon Linux 2023 (first-party SSM agent + Docker packaging).
  source_ami_filter {
    filters = {
      name                = "al2023-ami-2023.*-x86_64"
      root-device-type    = "ebs"
      virtualization-type = "hvm"
    }
    owners      = ["amazon"]
    most_recent = true
  }

  tags = {
    "gt:ami"  = "worker-ec2"
    "gt:role" = "polecat-worker"
  }
}

build {
  sources = ["source.amazon-ebs.worker"]

  # --- Packages: dockerd, amazon-ssm-agent, git -----------------------------
  provisioner "shell" {
    inline = [
      "sudo dnf install -y docker git amazon-ssm-agent",
      "sudo systemctl enable docker amazon-ssm-agent",
    ]
  }

  # --- Users: shared gt group + non-root gt-agent UID -----------------------
  # gt-agent is the dedicated non-root UID required by the native/host-net
  # IMDS firewall (EC2 spec §10). The shared gt group keeps the worktree
  # writable across the gt-worker-agent/agent UID boundary (core §6.1).
  provisioner "shell" {
    inline = [
      "sudo groupadd --system gt",
      "sudo useradd --system --gid gt --groups docker --create-home --shell /sbin/nologin gt-agent",
      "sudo install -d -o root -g gt -m 0775 /opt/gt",
    ]
  }

  # --- gt-worker-agent bootstrapper (placeholder) ---------------------------
  # A dumb oneshot: waits for the SSM-injected gt-worker-agent binary to land
  # in /opt/gt, verifies it, then starts gt-worker-agent.service. All
  # version-sensitive logic arrives at boot; replace this stub with the real
  # bootstrapper script when it exists.
  provisioner "shell" {
    inline = [
      "sudo tee /usr/local/sbin/gt-worker-bootstrap >/dev/null <<'EOF'",
      "#!/bin/sh",
      "# PLACEHOLDER: wait for SSM injection of /opt/gt/gt-worker-agent,",
      "# verify checksum, then: systemctl start gt-worker-agent.service",
      "exit 0",
      "EOF",
      "sudo chmod 0755 /usr/local/sbin/gt-worker-bootstrap",
    ]
  }

  # --- systemd unit stubs ----------------------------------------------------
  provisioner "shell" {
    inline = [
      "sudo tee /etc/systemd/system/gt-worker-bootstrap.service >/dev/null <<'EOF'",
      "[Unit]",
      "Description=gastown worker bootstrapper (waits for SSM-injected binaries)",
      "After=network-online.target amazon-ssm-agent.service docker.service",
      "Wants=network-online.target",
      "",
      "[Service]",
      "Type=oneshot",
      "ExecStart=/usr/local/sbin/gt-worker-bootstrap",
      "RemainAfterExit=yes",
      "",
      "[Install]",
      "WantedBy=multi-user.target",
      "EOF",
      "sudo tee /etc/systemd/system/gt-worker-agent.service >/dev/null <<'EOF'",
      "[Unit]",
      "Description=gastown worker agent (relay, checkpoint loop, interrupt handler)",
      "After=gt-worker-bootstrap.service docker.service",
      "Requires=docker.service",
      "",
      "[Service]",
      "# Injected at boot over SSM; not baked into the AMI.",
      "ExecStart=/opt/gt/gt-worker-agent",
      "Restart=on-failure",
      "# Root: manages dockerd, worktree checkpointing, and the UID-scoped",
      "# IMDS firewall; drops privileges to gt-agent for native-mode agents.",
      "User=root",
      "",
      "[Install]",
      "WantedBy=multi-user.target",
      "EOF",
      "sudo systemctl enable gt-worker-bootstrap.service",
    ]
  }
}
