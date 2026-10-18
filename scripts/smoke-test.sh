#!/usr/bin/env bash
# Image smoke test (run by CI; needs Docker on Linux).
# Usage: scripts/smoke-test.sh <image>
# Checks the image's default user is not root, starts it as an arbitrary non-root
# UID/GID with a throwaway Caller and an empty Upload Root, waits for its HEALTHCHECK to
# report healthy, then checks GET /healthz is 200 and the API needs a Credential.
set -euo pipefail

image="${1:?usage: $0 <image>}"

fail() { echo "smoke: FAIL: $*" >&2; exit 1; }

default_user="$(docker image inspect -f '{{.Config.User}}' "$image")"
case "$default_user" in
  "" | root | 0 | 0:* | root:*) fail "image default user is root (\"$default_user\")" ;;
esac
echo "smoke: ok: image default user is \"$default_user\""

uid=12345
gid=12345
# Throwaway Credential, valid only for this container.
token="$(openssl rand -hex 32)"
cid="$(docker run -d --platform linux/amd64 --user "$uid:$gid" -p 127.0.0.1::8080 \
  -e "NAS_FILE_API_CALLERS=smoke:$token" \
  --tmpfs "/data:uid=$uid,gid=$gid" \
  "$image")"

cleanup() {
  status=$?
  if [[ $status -ne 0 ]]; then
    echo "--- container logs ---" >&2
    docker logs "$cid" >&2 || true
  fi
  docker rm -f "$cid" > /dev/null 2>&1 || true
  exit $status
}
trap cleanup EXIT

# The image has no shell, so read the process's IDs from the host (same PID namespace view).
pid="$(docker inspect -f '{{.State.Pid}}' "$cid")"
ids="$(ps -o uid=,gid= -p "$pid" | awk '{print $1":"$2}')"
[[ "$ids" == "$uid:$gid" ]] || fail "container runs as $ids, want $uid:$gid"
echo "smoke: ok: container runs as $ids"

health=""
for _ in $(seq 1 30); do
  health="$(docker inspect -f '{{.State.Health.Status}}' "$cid")"
  [[ "$health" == "healthy" ]] && break
  [[ "$(docker inspect -f '{{.State.Running}}' "$cid")" == "true" ]] || fail "container exited"
  sleep 1
done
[[ "$health" == "healthy" ]] || fail "container health is \"$health\" after 30 s"
echo "smoke: ok: container healthcheck is healthy"

port="$(docker port "$cid" 8080/tcp | head -n1 | sed 's/.*://')"
code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/healthz")"
[[ "$code" == "200" ]] || fail "GET /healthz answered $code, want 200"
echo "smoke: ok: GET /healthz is 200"

code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/api/v1/folders/")"
[[ "$code" == "401" ]] || fail "GET /api/v1/folders/ without a Credential answered $code, want 401"
echo "smoke: ok: GET /api/v1/folders/ without a Credential is 401"

code="$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $token" "http://127.0.0.1:$port/api/v1/folders/")"
[[ "$code" == "200" ]] || fail "GET /api/v1/folders/ with a Credential answered $code, want 200"
echo "smoke: ok: GET /api/v1/folders/ with a Credential is 200"
echo "smoke: all checks passed"
