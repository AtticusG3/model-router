#!/usr/bin/env bash
# Smoke-test launcher: starts router B, router A, and cleans up.
# Usage: test/run-smoke.sh {start|stop|status}
set -u
cd "$(dirname "$0")/.."
ROOT="$(pwd)"
PIDDIR="$ROOT/test/.pids"
mkdir -p "$PIDDIR"

start_one() {
  local name="$1" cfg="$2" extra="${3:-}"
  setsid nohup "$ROOT/build/model-router" -config "$cfg" -node "$name" $extra \
    > "$PIDDIR/$name.log" 2>&1 < /dev/null &
  echo $! > "$PIDDIR/$name.pid"
  echo "started $name (pid $!)"
}

stop_one() {
  local name="$1"
  if [ -f "$PIDDIR/$name.pid" ]; then
    kill "$(cat "$PIDDIR/$name.pid")" 2>/dev/null
    rm -f "$PIDDIR/$name.pid"
    echo "stopped $name"
  fi
}

case "${1:-start}" in
  start)
    start_one router-b test/router-b.yaml
    sleep 1
    start_one router-a test/router-a.yaml "-verbose"
    sleep 2
    ;;
  stop)
    stop_one router-a
    stop_one router-b
    pkill -f "$ROOT/build/fake-model" 2>/dev/null
    ;;
  status)
    for n in router-a router-b; do
      if [ -f "$PIDDIR/$n.pid" ] && kill -0 "$(cat "$PIDDIR/$n.pid")" 2>/dev/null; then
        echo "$n: running"
      else
        echo "$n: stopped"
      fi
    done
    ;;
esac
