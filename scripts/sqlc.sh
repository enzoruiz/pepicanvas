#!/usr/bin/env sh
# Runs the pinned sqlc release against backend/sqlc.yaml.
# Usage: scripts/sqlc.sh [generate|diff|vet]
set -eu

image="sqlc/sqlc:1.31.1@sha256:70f53171d27b2424e9358869975455a6e955a5aa8e58a998a270a6e34e525537"
root=$(cd "$(dirname "$0")/.." && pwd)

exec docker run --rm --user "$(id -u):$(id -g)" \
	--volume "$root/backend:/src" --workdir /src \
	"$image" "${1:-generate}"
