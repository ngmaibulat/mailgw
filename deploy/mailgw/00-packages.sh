#!/bin/bash
# Edge (mailgw) node — host debugging tools only. The app runs in Docker, so no
# Node/Python toolchain is needed on the host.
set -euo pipefail

sudo apt-get update
sudo apt-get install -y swaks tcpdump vim
