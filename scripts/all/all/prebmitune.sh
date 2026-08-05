#!/bin/bash
# prebmitune.sh for all/all
# 2026.08.04
#
# Dispatcher for STREAMER_APP=scripts/all/all. The m3u passes the channel
# as device~provider~channel (e.g. firetv~fubo~12719) so a single ah4c
# instance can tune across multiple device/provider script sets. This
# script splits that value and hands off to the real
# scripts/<device>/<provider>/prebmitune.sh.

#Debug on if uncommented
set -x

#Global
streamerIP="$1"
combined="$2"
device=$(echo "$combined" | awk -F~ '{print $1}')
provider=$(echo "$combined" | awk -F~ '{print $2}')
channelID=$(echo "$combined" | cut -d'~' -f3-)
scriptDir="$(cd "$(dirname "$0")" && pwd)"
targetScript="$scriptDir/../../$device/$provider/prebmitune.sh"

#Trap end of script run
finish() {
  echo "prebmitune.sh (all/all) is exiting for $streamerIP with exit code $?"
}

trap finish EXIT

#Reject anything that isn't a bare directory name before it touches a path
validateDeviceProvider() {
  if [[ ! "$device" =~ ^[A-Za-z0-9_-]+$ ]] || [[ ! "$provider" =~ ^[A-Za-z0-9_-]+$ ]]; then
    echo "Invalid device/provider parsed from channel argument: $combined"
    exit 1
  fi
}

dispatch() {
  if [[ ! -x "$targetScript" ]]; then
    echo "No prebmitune.sh found for device=$device provider=$provider ($targetScript)"
    exit 1
  fi

  echo "Dispatching prebmitune.sh to $device/$provider for $streamerIP"
  exec "$targetScript" "$streamerIP" "$channelID"
}

main() {
  validateDeviceProvider
  dispatch
}

main
