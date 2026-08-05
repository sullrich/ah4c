#!/bin/bash
# bmitune.sh for all/all
# 2026.08.04
#
# Dispatcher for STREAMER_APP=scripts/all/all. The m3u passes the channel
# as device~provider~channel (e.g. firetv~fubo~12719) so a single ah4c
# instance can tune across multiple device/provider script sets. This
# script splits that value and hands off to the real
# scripts/<device>/<provider>/bmitune.sh. Whatever remains after the
# device~provider~ prefix is passed through untouched, so providers whose
# own channel field is itself composite (e.g. dtvdeeplinks' channelName~channelID)
# keep working unmodified.

#Debug on if uncommented
set -x

#Global
combined="$1"
streamerIP="$2"
device=$(echo "$combined" | awk -F~ '{print $1}')
provider=$(echo "$combined" | awk -F~ '{print $2}')
channelID=$(echo "$combined" | cut -d'~' -f3-)
scriptDir="$(cd "$(dirname "$0")" && pwd)"
targetScript="$scriptDir/../../$device/$provider/bmitune.sh"

#Trap end of script run
finish() {
  echo "bmitune.sh (all/all) is exiting for $streamerIP with exit code $?"
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
    echo "No bmitune.sh found for device=$device provider=$provider ($targetScript)"
    exit 1
  fi

  echo "Dispatching bmitune.sh to $device/$provider for $streamerIP, channel $channelID"
  exec "$targetScript" "$channelID" "$streamerIP"
}

main() {
  validateDeviceProvider
  dispatch
}

main
