#!/usr/bin/env bash
# Clones (or updates) github.com/leil-io/saunafs-container and builds all
# required images locally. Run from the project root.
set -euo pipefail

REPO_URL="https://github.com/leil-io/saunafs-container.git"
CLONE_DIR="${TMPDIR:-/tmp}/saunafs-container"
IMAGES=(saunafs-master saunafs-chunkserver saunafs-client saunafs-metalogger saunafs-cgiserver)

echo "==> Fetching saunafs-container sources..."
if [ -d "$CLONE_DIR/.git" ]; then
  git -C "$CLONE_DIR" pull --ff-only
else
  git clone --depth 1 "$REPO_URL" "$CLONE_DIR"
fi

echo "==> Building saunafs-base..."
docker build \
  -f "$CLONE_DIR/saunafs-base/Dockerfile" \
  -t saunafs-base:latest \
  "$CLONE_DIR/saunafs-base/"

for img in "${IMAGES[@]}"; do
  echo "==> Building ${img}..."
  docker build \
    -f "$CLONE_DIR/${img}/Dockerfile" \
    -t "${img}:latest" \
    "$CLONE_DIR/${img}/"
done

echo "==> Done. Built images:"
for img in "${IMAGES[@]}"; do
  docker images --format "  {{.Repository}}:{{.Tag}}\t{{.ID}}\t{{.Size}}" "${img}"
done
