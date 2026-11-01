#!/usr/bin/env bash
# run_container.sh: Run the benchmark container with strict cgroups limits.
# Usage:
#   ./scripts/run_container.sh <IMAGE_NAME> [PORT] [CPUS] [MEMORY]
# Example:
#   ./scripts/run_container.sh uring-http:latest 8080 1.0 128m

set -euo pipefail

IMAGE="${1:-uring-http:latest}"
PORT="${2:-8080}"
CPUS="${3:-1.0}"
MEMORY="${4:-128m}"
CONTAINER_NAME="uring-bench-target"

# Stop existing container if running
if docker ps -a --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
    echo "Stopping and removing existing container: $CONTAINER_NAME"
    docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
fi

echo "=========================================================="
echo " Starting Benchmark Container"
echo " Image:        $IMAGE"
echo " Port:         $PORT"
echo " CPUs:         $CPUS"
echo " Memory Cap:   $MEMORY (Swap: $MEMORY)"
echo "=========================================================="

docker run -d \
    --name "$CONTAINER_NAME" \
    --cpus="$CPUS" \
    --memory="$MEMORY" \
    --memory-swap="$MEMORY" \
    -p "${PORT}:8080" \
    "$IMAGE"

echo ""
echo "Container started successfully!"
echo "View logs:      docker logs -f $CONTAINER_NAME"
echo "Inspect stats:  docker stats $CONTAINER_NAME"
echo "Monitor script: ./scripts/monitor.sh \$(docker inspect -f '{{.State.Pid}}' $CONTAINER_NAME) 1"
echo "Stop container: docker rm -f $CONTAINER_NAME"
