#!/usr/bin/env bash
# monitor.sh: Non-invasive, high-precision telemetry sampler for Linux processes.
# Usage:
#   ./scripts/monitor.sh <PID_or_PROCESS_NAME> [INTERVAL_SEC]
# Examples:
#   ./scripts/monitor.sh http-variant 2
#   ./scripts/monitor.sh 36846 1

set -euo pipefail

TARGET="${1:-}"
INTERVAL="${2:-1}"

if [ -z "$TARGET" ]; then
    echo "Usage: $0 <PID_or_NAME> [INTERVAL_SEC]"
    echo "Example: $0 http-variant 1"
    exit 1
fi

# If TARGET is not an integer PID, resolve the actual server binary PID
if ! [[ "$TARGET" =~ ^[0-9]+$ ]]; then
    PID=$(pgrep -f "$TARGET" 2>/dev/null | while read -r p; do
        if [ "$p" != "$$" ] && [ -d "/proc/$p" ]; then
            CMD=$(tr '\0' ' ' < "/proc/$p/cmdline" 2>/dev/null || true)
            if [[ "$CMD" != *monitor.sh* ]]; then
                echo "$p"
            fi
        fi
    done | tail -n1 || true)

    if [ -z "$PID" ]; then
        echo "Error: No active process found matching '$TARGET'"
        exit 1
    fi
else
    PID="$TARGET"
fi

if [ ! -d "/proc/$PID" ]; then
    echo "Error: Process $PID not found in /proc"
    exit 1
fi

CMDLINE=$(tr '\0' ' ' < "/proc/$PID/cmdline" 2>/dev/null | cut -c1-60)
echo "=== Monitoring PID $PID: $CMDLINE (Interval: ${INTERVAL}s) ==="
printf "%-10s | %-10s | %-12s | %-12s | %-8s\n" "TIME" "RSS (MB)" "VOL_CTX_SW" "INVOL_CTX_SW" "FD_COUNT"
echo "----------------------------------------------------------------------"

while [ -d "/proc/$PID" ]; do
    TIMESTAMP=$(date +"%H:%M:%S")
    STATUS_FILE="/proc/$PID/status"
    
    if [ ! -f "$STATUS_FILE" ]; then
        break
    fi

    # Exact line match with caret ^ to avoid matching both voluntary and nonvoluntary
    RSS_KB=$(grep -E "^VmRSS:" "$STATUS_FILE" | awk '{print $2}' || echo "0")
    RSS_MB=$(awk "BEGIN {printf \"%.2f\", ${RSS_KB:-0}/1024}")
    
    VOL_SW=$(grep -E "^voluntary_ctxt_switches:" "$STATUS_FILE" | awk '{print $2}' || echo "0")
    INVOL_SW=$(grep -E "^nonvoluntary_ctxt_switches:" "$STATUS_FILE" | awk '{print $2}' || echo "0")
    
    FD_COUNT=$(ls -1 "/proc/$PID/fd" 2>/dev/null | wc -l || echo "0")

    printf "%-10s | %-10s | %-12s | %-12s | %-8s\n" "$TIMESTAMP" "$RSS_MB" "$VOL_SW" "$INVOL_SW" "$FD_COUNT"
    
    sleep "$INTERVAL"
done

echo ""
echo "=== Process $PID terminated. Telemetry stopped. ==="
