#!/bin/bash
# stopbmitune.sh for atv/spectrum
# 2026.09.22

set -e

# Channels DVR invocation contract: stopbmitune.sh <atvHost> <channelId>
#   $1 = atvHost    (IP of the Apple TV assigned as this tuner)
#   $2 = channelId  (Gracenote/TMS station ID, from tvc-guide-stationid in M3U)
# NOTE: argument order is REVERSED from bmitune.sh - this is the Channels DVR
# pre/stop contract, not a bug. Do not "align" this with bmitune.sh's order.
atvHost="$1"
channelId="$2"

pyatvCredentials="/root/.android/.pyatv.conf"
ATV_CMD="/usr/local/bin/atvremote --storage-filename ${pyatvCredentials} -s ${atvHost}"

# Tunable delays - measure per-ATV, don't assume Cox's timing applies here.
FC_SHORT_DELAY=0.6
SWIPE_PASSES=3   # start smaller than Cox's 5; raise only if one pass isn't enough

echo "📺 Force Quit Spectrum App on ${atvHost} (was tuned to ${channelId})"

echo "🔁 Opening app switcher..."
eval "$ATV_CMD home home" >/dev/null 2>&1 || true
sleep "$FC_SHORT_DELAY"

# NOTE: no "left" press here - Spectrum should already be the frontmost,
# focused card since it's the most-recently-used app on this ATV. If testing
# shows the wrong app gets swiped, that's the signal to add navigation back.

i=1
while [ "$i" -le "$SWIPE_PASSES" ]; do
  echo "🧹 Swiping up to force-quit the app... (Pass $i)"
  eval "$ATV_CMD up up" >/dev/null 2>&1 || true
  sleep "$FC_SHORT_DELAY"
  i=$((i + 1))
done

echo "🏡 Returning to Home Screen..."
eval "$ATV_CMD home" >/dev/null 2>&1 || true
sleep "$FC_SHORT_DELAY"

echo "✅ Spectrum app force-quit, ATV in idle state"
