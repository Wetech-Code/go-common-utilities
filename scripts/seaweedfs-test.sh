#!/usr/bin/env bash
# Starts a disposable SeaweedFS S3 gateway in Docker with random throwaway
# credentials, runs the s3 integration test and the consumer check against it,
# then removes the container. Needs Docker, curl and openssl.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

image="${SEAWEEDFS_IMAGE:-chrislusf/seaweedfs:4.52}"
name="gcu-seaweedfs-test"
dir="$(mktemp -d)"
trap 'docker rm -f "$name" >/dev/null 2>&1 || true; rm -rf "$dir"' EXIT

access="$(openssl rand -hex 8)"; secret="$(openssl rand -hex 16)"
cat > "$dir/s3.json" <<JSON
{"identities":[{"name":"test","credentials":[{"accessKey":"$access","secretKey":"$secret"}],"actions":["Admin","Read","Write","List","Tagging"]}]}
JSON

docker run -d --name "$name" -p 8333:8333 -p 9333:9333 \
  -v "$dir/s3.json:/etc/seaweedfs/s3.json:ro" \
  "$image" server -dir=/data -s3 -s3.port=8333 -s3.config=/etc/seaweedfs/s3.json >/dev/null

for i in $(seq 1 60); do
  if curl -s http://localhost:9333/dir/assign | grep -q '"fid"' && curl -s -o /dev/null http://localhost:8333/; then break; fi
  [ "$i" = 60 ] && { docker logs "$name" | tail -50; echo "SeaweedFS not ready" >&2; exit 1; }
  sleep 1
done
docker exec "$name" weed version

export GCU_TEST_S3_ENDPOINT=http://localhost:8333 GCU_TEST_S3_ACCESS_KEY="$access" GCU_TEST_S3_SECRET_KEY="$secret"
go test -tags integration -race -count=1 -v -run Integration ./s3 | tee "$dir/out.log"
! grep -e '--- SKIP' "$dir/out.log"   # a skipped test proves nothing
scripts/consumer-check.sh
