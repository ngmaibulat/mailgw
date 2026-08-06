#!/bin/bash
# Bump the patch version in VERSION. Separate from the build on purpose, so a
# build never mutates the tree — the same rule logservice-go and mailgw-go
# follow and the Node packages do not.
set -euo pipefail
cd "$(dirname "$0")"

CUR=$(tr -d ' \n' < VERSION)
IFS=. read -r MAJOR MINOR PATCH <<< "$CUR"
NEW="${MAJOR}.${MINOR}.$((PATCH + 1))"

echo "$NEW" > VERSION
echo "${CUR} -> ${NEW}"
