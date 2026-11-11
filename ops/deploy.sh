#!/usr/bin/env bash
set -euo pipefail

tag=${1:?release tag required}
[[ "$tag" =~ ^v2\.[0-9]+\.[0-9]+([.-][a-zA-Z0-9.-]+)?$ ]]
: "${DEPLOY_HOST:?}" "${DEPLOY_KEY:?}" "${DEPLOY_KNOWN_HOSTS:?}" "${GITHUB_REPOSITORY:?}"
[[ "$DEPLOY_HOST" =~ ^[a-zA-Z0-9_.-]+@[a-zA-Z0-9_.-]+$ ]]
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
chmod 700 "$tmp"
printf '%s\n' "$DEPLOY_KEY" > "$tmp/key"
printf '%s\n' "$DEPLOY_KNOWN_HOSTS" > "$tmp/known_hosts"
chmod 600 "$tmp/key"
ssh_opts=(-i "$tmp/key" -o "UserKnownHostsFile=$tmp/known_hosts" -o StrictHostKeyChecking=yes -o BatchMode=yes)
ssh "${ssh_opts[@]}" "$DEPLOY_HOST" "mkdir -p image-service/releases/$tag"
tar -czf - docker-compose.yml ops | ssh "${ssh_opts[@]}" "$DEPLOY_HOST" "tar -xzf - -C image-service/releases/$tag"
repository=${GITHUB_REPOSITORY,,}
[[ "$repository" =~ ^[a-z0-9_.-]+/[a-z0-9_.-]+$ ]]
ssh "${ssh_opts[@]}" "$DEPLOY_HOST" "bash -s -- '$tag' '$repository'" <<'REMOTE'
set -euo pipefail
cd "$HOME/image-service"
tag=$1
repository=$2
test -f .env
compose() {
  local release=$1
  shift
  GO_IMAGE="ghcr.io/$repository-go:$release" PROCESSOR_IMAGE="ghcr.io/$repository-processor:$release" \
    docker compose -p image-service --env-file "$PWD/.env" -f "releases/$release/docker-compose.yml" "$@"
}
previous=$(cat current 2>/dev/null || true)
compose "$tag" pull
if compose "$tag" up -d --no-build --wait --wait-timeout 180; then
  printf '%s\n' "$tag" > current
elif [[ -n "$previous" ]]; then
  echo "Deployment failed, restoring $previous" >&2
  compose "$previous" up -d --no-build --wait --wait-timeout 180
  exit 1
else
  echo "First deployment failed" >&2
  exit 1
fi
REMOTE
