#!/usr/bin/env bash
# Runs the integration tests for all four providers against disposable Docker
# containers (docker-compose.yml), then the independent consumer check, then
# removes the containers. Needs Docker with the compose plugin, curl, openssl, Go.
#
#   scripts/integration.sh
#
# Credentials for SeaweedFS are random for every run and never written into the repo.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

command -v docker >/dev/null || { echo "Docker is required for this script." >&2; exit 1; }
docker compose version >/dev/null || { echo "The docker compose plugin is required." >&2; exit 1; }

dir="$(mktemp -d)"
cleanup() { docker compose down -v >/dev/null 2>&1 || true; rm -rf "$dir"; }
trap cleanup EXIT

access="$(openssl rand -hex 8)"
secret="$(openssl rand -hex 16)"
cat > "$dir/s3.json" <<JSON
{"identities":[{"name":"test","credentials":[{"accessKey":"$access","secretKey":"$secret"}],"actions":["Admin","Read","Write","List","Tagging"]}]}
JSON
export GCU_S3_CONFIG="$dir/s3.json"

echo "== starting containers"
docker compose up -d --wait postgres mongodb redis seaweedfs

echo "== waiting for the SeaweedFS S3 gateway"
for i in $(seq 1 60); do
  if curl -s http://localhost:9333/dir/assign | grep -q '"fid"' && curl -s -o /dev/null http://localhost:8333/; then break; fi
  if [ "$i" = 60 ]; then docker compose logs seaweedfs | tail -50; echo "SeaweedFS not ready" >&2; exit 1; fi
  sleep 1
done

echo "== server versions"
docker compose exec -T postgres postgres --version
docker compose exec -T mongodb mongod --version | head -1
docker compose exec -T redis redis-server --version
docker compose exec -T seaweedfs weed version | head -1

export GCU_TEST_POSTGRES_URL='postgres://postgres@localhost:15432/postgres?sslmode=disable'
export GCU_TEST_MONGODB_URI='mongodb://localhost:27117/?directConnection=true'
export GCU_TEST_REDIS_URL='redis://localhost:16379/0'
export GCU_TEST_S3_ENDPOINT='http://localhost:8333'
export GCU_TEST_S3_ACCESS_KEY="$access"
export GCU_TEST_S3_SECRET_KEY="$secret"

echo "== integration tests"
go test -tags integration -race -count=1 -v -run Integration ./... | tee "$dir/out.log"
# A skipped test proves nothing.
if grep -e '--- SKIP' "$dir/out.log"; then echo "an integration test was skipped" >&2; exit 1; fi

echo "== independent consumer against the same servers"
scripts/consumer-check.sh

echo "integration tests passed"
