#!/bin/bash
# loadtest.sh
# Requires 'hey' (https://github.com/rakyll/hey) or 'wrk' (https://github.com/wg/wrk)
#
# This script sends high-concurrency traffic to PeakShield to benchmark its
# reverse proxying, rate limiting, and waiting room performance.
#
# Usage:
#   1. Start PeakShield in a separate terminal: PEAKSHIELD_LISTEN=:8080 go run .
#   2. Run this script: ./scripts/loadtest.sh

TARGET_URL="http://localhost:8080"
CONCURRENCY=500
REQUESTS=50000

echo "Starting load test against $TARGET_URL"
echo "Concurrency: $CONCURRENCY, Total Requests: $REQUESTS"
echo ""

if command -v hey &> /dev/null; then
    hey -c $CONCURRENCY -n $REQUESTS $TARGET_URL
elif command -v wrk &> /dev/null; then
    # wrk runs for a duration rather than request count by default, 
    # but provides higher throughput generation.
    wrk -t 4 -c $CONCURRENCY -d 10s $TARGET_URL
else
    echo "Error: Neither 'hey' nor 'wrk' is installed."
    echo "Install hey: go install github.com/rakyll/hey@latest"
    echo "Install wrk: brew install wrk (mac) or apt-get install wrk (ubuntu)"
    exit 1
fi
