#!/usr/bin/env bash
#
# Run the SDK conformance suite: build the gateway with the scripted mock
# backend, start it, drive it with the real openai and anthropic SDKs, stop it.
#
#   scripts/conformance/run.sh
#
# Needs Python 3 and network access the first time, to install the two SDKs into
# a virtualenv under .venv-conformance/. Set PORT to move the listener.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../.." && pwd)"
port="${PORT:-8799}"
key="sk-conformance"
venv="$root/.venv-conformance"
workdir="$(mktemp -d)"
bin="$workdir/agent2api"

cd "$root"

echo "==> building with -tags conformance"
go build -tags conformance -o "$bin" ./cmd/agent2api

if [ ! -x "$venv/bin/python" ]; then
  echo "==> creating $venv"
  python3 -m venv "$venv"
fi
if ! "$venv/bin/python" -c "import openai, anthropic" 2>/dev/null; then
  echo "==> installing the openai and anthropic SDKs"
  "$venv/bin/pip" install --quiet --upgrade pip
  "$venv/bin/pip" install --quiet openai anthropic
fi

log="$workdir/gateway.log"
"$bin" serve -c "$here/config.yaml" --port "$port" >"$log" 2>&1 &
server=$!
cleanup() {
  kill "$server" 2>/dev/null || true
  wait "$server" 2>/dev/null || true
  rm -rf "$workdir"
}
trap cleanup EXIT

echo "==> waiting for the gateway on 127.0.0.1:$port"
for _ in $(seq 1 50); do
  if curl -sf "http://127.0.0.1:$port/healthz" >/dev/null; then
    break
  fi
  if ! kill -0 "$server" 2>/dev/null; then
    echo "the gateway exited before it was ready:" >&2
    cat "$log" >&2
    exit 1
  fi
  sleep 0.1
done

set +e
"$venv/bin/python" "$here/conformance.py" --base-url "http://127.0.0.1:$port" --api-key "$key" "$@"
status=$?
set -e

if [ "$status" -ne 0 ]; then
  echo
  echo "==> gateway log"
  cat "$log"
fi
exit "$status"
