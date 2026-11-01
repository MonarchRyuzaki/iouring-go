#!/usr/bin/env bash
# load_test.sh: Benchmark harness using wrk and curl to hit the /ping endpoint.
# Usage:
#   ./scripts/load_test.sh smoke [URL]
#   ./scripts/load_test.sh throughput [URL] [DURATION] [CONNECTIONS]
#   ./scripts/load_test.sh stress [URL] [DURATION] [CONNECTIONS]

set -euo pipefail

MODE="${1:-throughput}"
TARGET_URL="${2:-http://127.0.0.1:8081/ping}"
DURATION="${3:-10s}"
CONNECTIONS="${4:-100}"
THREADS="4"

echo "=========================================================="
echo " Mode:        $MODE"
echo " Target:      $TARGET_URL"
echo "=========================================================="

case "$MODE" in
    smoke)
        echo "--> Sending single HTTP GET via curl..."
        curl -i "$TARGET_URL"
        echo ""
        ;;

    throughput)
        echo "--> Running throughput test (Threads: $THREADS, Conns: $CONNECTIONS, Duration: $DURATION)..."
        wrk -t"$THREADS" -c"$CONNECTIONS" -d"$DURATION" --latency "$TARGET_URL"
        ;;

    stress)
        CONNECTIONS="${4:-1000}"
        DURATION="${3:-30s}"
        echo "--> Running high-concurrency stress test (Threads: $THREADS, Conns: $CONNECTIONS, Duration: $DURATION)..."
        wrk -t"$THREADS" -c"$CONNECTIONS" -d"$DURATION" --latency "$TARGET_URL"
        ;;

    *)
        echo "Unknown mode: $MODE"
        echo "Available modes: smoke, throughput, stress"
        exit 1
        ;;
esac
