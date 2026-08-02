#!/bin/bash
# Edge node Docker install — delegates to the shared installer.
set -euo pipefail

bash "$(dirname "$0")/../common/install-docker.sh"
