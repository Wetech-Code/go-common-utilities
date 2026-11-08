#!/usr/bin/env bash
# Runs the integration tests for all providers (PostgreSQL, MongoDB, Redis, SeaweedFS,
# Loki, Tempo, Prometheus and Grafana) against disposable Docker containers (docker-compose.yml), then the independent consumer check, then
# removes the containers. Needs Docker with the compose plugin, curl, openssl, Go.
#
#   scripts/integration.sh
#
# Credentials for SeaweedFS are random for every run and never written into the repo.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

command -v docker >/dev/null || { echo "Docker is required for this script." >&2; exit 1; }
docker compose version >/dev/null || { echo "The docker compose plugin is required." >&2; exit 1; }

# Inside the repo (gitignored), not /tmp or /var/folders: Colima, Lima and similar
# engines only share your home directory with the VM, and a bind mount of an
# unshared path silently becomes an empty directory.
dir="$(mktemp -d "$PWD/.integration-XXXXXX")"
cleanup() { docker compose down -v >/dev/null 2>&1 || true; rm -rf "$dir"; }
trap cleanup EXIT

access="$(openssl rand -hex 8)"
secret="$(openssl rand -hex 16)"
cat > "$dir/s3.json" <<JSON
{"identities":[{"name":"test","credentials":[{"accessKey":"$access","secretKey":"$secret"}],"actions":["Admin","Read","Write","List","Tagging"]}]}
JSON
export GCU_S3_CONFIG="$dir/s3.json"
GCU_GRAFANA_PASSWORD="$(openssl rand -hex 12)"
export GCU_GRAFANA_PASSWORD

echo "== starting containers"
docker compose up -d --wait postgres mongodb redis seaweedfs loki tempo prometheus grafana

echo "== waiting for the SeaweedFS S3 gateway"
for i in $(seq 1 60); do
  if curl -s http://localhost:9333/dir/assign | grep -q '"fid"' && curl -s -o /dev/null http://localhost:8333/; then break; fi
  if [ "$i" = 60 ]; then docker compose logs seaweedfs | tail -50; echo "SeaweedFS not ready" >&2; exit 1; fi
  sleep 1
done

echo "== waiting for Loki"
for i in $(seq 1 90); do
  if curl -s http://localhost:13100/ready | grep -q '^ready'; then break; fi
  if [ "$i" = 90 ]; then docker compose logs loki | tail -30; echo "Loki not ready" >&2; exit 1; fi
  sleep 1
done

echo "== waiting for Tempo and Prometheus"
for i in $(seq 1 120); do
  if curl -s http://localhost:13200/ready | grep -q '^ready' && curl -s http://localhost:19090/-/ready | grep -qi 'ready'; then break; fi
  if [ "$i" = 120 ]; then docker compose logs tempo prometheus | tail -40; echo "Tempo or Prometheus not ready" >&2; exit 1; fi
  sleep 1
done

echo "== server versions (informational; a failure here does not fail the run)"
{
  docker compose exec -T postgres postgres --version
  docker compose exec -T mongodb mongod --version | head -1
  docker compose exec -T redis redis-server --version
  docker compose exec -T seaweedfs weed version | head -1
} || true

export GCU_TEST_POSTGRES_URL='postgres://postgres@localhost:15432/postgres?sslmode=disable'
export GCU_TEST_MONGODB_URI='mongodb://localhost:27117/?directConnection=true'
export GCU_TEST_REDIS_URL='redis://localhost:16379/0'
export GCU_TEST_S3_ENDPOINT='http://localhost:8333'
export GCU_TEST_S3_ACCESS_KEY="$access"
export GCU_TEST_S3_SECRET_KEY="$secret"
export GCU_TEST_LOKI_URL='http://localhost:13100'
export GCU_TEST_GRAFANA_URL='http://localhost:13000'
export GCU_TEST_TEMPO_OTLP_URL='http://localhost:14318'
export GCU_TEST_PROMETHEUS_URL='http://localhost:19090'
export GCU_TEST_GRAFANA_PASSWORD="$GCU_GRAFANA_PASSWORD"

echo "== integration tests"
# The redis tests start their own disposable servers: redis:8 containers for most,
# including the module commands (JSON, Search, TimeSeries, Bloom, ...).
go test -tags integration -race -count=1 -v -run Integration ./... | tee "$dir/out.log"

# The two-node cluster test needs a local redis-server binary (Docker port mappings
# cannot join cluster nodes). Run it when one is installed.
if command -v redis-server >/dev/null; then
  GCU_TEST_REDIS_SERVER="$(command -v redis-server)" go test -tags integration -race -count=1 -v -run 'IntegrationClusterMultiNode' ./redis | tee -a "$dir/out.log"
else
  echo "WARNING: redis-server not installed, so TestIntegrationClusterMultiNode (ClusterMeet/Replicate/Failover/Forget) was NOT run." >&2
fi

# A skipped test proves nothing. The only skip allowed is that multi-node test when no binary exists.
if grep -e '--- SKIP' "$dir/out.log" | grep -v 'ClusterMultiNode'; then echo "an integration test was skipped" >&2; exit 1; fi

echo "== independent consumer against the same servers"
scripts/consumer-check.sh

echo "integration tests passed"
