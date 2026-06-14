#!/bin/bash
# Install Docker Engine + CLI + Buildx + Compose plugins on Ubuntu (24.04/26.04)
# using the current official apt repository (signed-by keyring). Shared by both
# the core and edge node deploys.
set -euo pipefail

# 1. Prereqs (apt-transport-https / software-properties-common are no longer
#    needed on modern Ubuntu).
sudo apt-get update
sudo apt-get install -y ca-certificates curl

# 2. Official Docker GPG key in the recommended keyrings location.
sudo install -m 0755 -d /etc/apt/keyrings
sudo curl -fsSL https://download.docker.com/linux/ubuntu/gpg \
    -o /etc/apt/keyrings/docker.asc
sudo chmod a+r /etc/apt/keyrings/docker.asc

# 3. Repo, pinned to this host's arch + release codename.
echo \
  "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] \
https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
  | sudo tee /etc/apt/sources.list.d/docker.list > /dev/null

# 4. Engine + the Buildx and Compose plugins (the old script installed only
#    docker-ce and was missing both).
sudo apt-get update
sudo apt-get install -y \
    docker-ce docker-ce-cli containerd.io \
    docker-buildx-plugin docker-compose-plugin

# 5. Run as a system service; let the invoking user use docker without sudo.
sudo systemctl enable --now docker
sudo usermod -aG docker "$USER"

docker --version
docker compose version

echo "Docker installed. Log out/in (or run 'newgrp docker') for group membership to take effect."
