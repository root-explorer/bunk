#!/usr/bin/env bash
# bunk end-to-end smoke test: hub + two agents + real docker, all locally.
# Exercises: pairing, E2E relay, docker proxy, port forwarding.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"
HUB_ADDR=127.0.0.1:18777
HUB_URL="ws://$HUB_ADDR/ws"
TOKEN="smoke-test-token"
PIDS=()
cleanup() {
  docker rm -f bunk-smoke-srv >/dev/null 2>&1 || true
  for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null || true; done
  rm -rf "$WORK"
}
trap cleanup EXIT

# remove any stale smoke container from a previously killed run
docker rm -f bunk-smoke-srv >/dev/null 2>&1 || true

echo "== building =="
(cd "$ROOT" && go build -o "$WORK/bunk" ./cmd/bunk && go build -o "$WORK/bunk-hub" ./cmd/bunk-hub)

echo "== starting hub =="
BUNK_HUB_TOKEN="$TOKEN" "$WORK/bunk-hub" -addr "$HUB_ADDR" -db "$WORK/hub.db" >"$WORK/hub.log" 2>&1 &
PIDS+=($!)
sleep 0.5

BUNK_HOME="$WORK/a" BUNK_HUB="$HUB_URL" BUNK_HUB_TOKEN="$TOKEN" "$WORK/bunk" daemon >"$WORK/a.log" 2>&1 &
PIDS+=($!)
BUNK_HOME="$WORK/b" BUNK_HUB="$HUB_URL" BUNK_HUB_TOKEN="$TOKEN" "$WORK/bunk" daemon >"$WORK/b.log" 2>&1 &
PIDS+=($!)
sleep 0.7

echo "== host (a) creates pairing code =="
CODE=$(BUNK_HOME="$WORK/a" "$WORK/bunk" pair --name host-a | grep -oE '[A-Z2-9]{3}-[A-Z2-9]{3}' | head -1)
echo "   code: $CODE"

echo "== guest (b) pairs =="
BUNK_HOME="$WORK/b" "$WORK/bunk" pair "$CODE" --name guest-b

echo "== machines =="
BUNK_HOME="$WORK/b" "$WORK/bunk" machines

echo "== guest points at host =="
BUNK_HOME="$WORK/b" "$WORK/bunk" use host-a

echo "== run a container on host-a through the tunnel (real docker) =="
BUNK_HOME="$WORK/b" "$WORK/bunk" --dry-run run --rm busybox:latest echo hello-from-bunk
BUNK_HOME="$WORK/b" "$WORK/bunk" run --rm busybox:latest echo hello-from-bunk

echo "== start a service + forward its port =="
# BUNK_NO_AUTO_FORWARD keeps the guest's forward port free on this same-host test
BUNK_NO_AUTO_FORWARD=1 BUNK_HOME="$WORK/b" "$WORK/bunk" run -d --name bunk-smoke-srv -p 18081:80 busybox:latest sh -c 'echo "bunk works" > /tmp/i.html; httpd -f -p 80 -h /tmp' >/dev/null
sleep 1
BUNK_HOME="$WORK/b" "$WORK/bunk" forward 18778:18081
BODY=$(curl -s --max-time 10 http://127.0.0.1:18778/i.html)
echo "   forwarded response: $BODY"
[ "$BODY" = "bunk works" ]

echo "== limits injected (docker inspect shows cpus/memory) =="
BUNK_HOME="$WORK/b" "$WORK/bunk" inspect --format '{{.HostConfig.NanoCpus}} {{.HostConfig.Memory}}' bunk-smoke-srv

echo "== revoke cuts the link =="
BUNK_HOME="$WORK/b" "$WORK/bunk" revoke host-a
sleep 0.3
if BUNK_HOME="$WORK/b" "$WORK/bunk" run --rm busybox:latest echo nope 2>"$WORK/revoke.err"; then
  echo "FAIL: run should have failed after revoke"
  exit 1
fi
echo "   revoke blocked: $(head -1 "$WORK/revoke.err")"

echo ""
echo "SMOKE TEST PASSED"
