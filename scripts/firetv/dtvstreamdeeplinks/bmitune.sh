#!/bin/bash
# bmitune.sh for firetv/dtvstreamdeeplinks
# 2026.08.17

#Debug on if uncommented
set -x

#Global
channelID=$(echo $1 | awk -F~ '{print $2}')
channelName=$(echo $1 | awk -F~ '{print $1}')
streamerIP="$2"
streamerNoPort="${streamerIP%%:*}"
adbTarget="adb -s $streamerIP"
packageName=com.att.tv

#Trap end of script run
finish() {
  echo "bmitune.sh is exiting for $streamerIP with exit code $?"
}

trap finish EXIT

updateReferenceFiles() {

  # Handle cases where stream_stopped or last_channel don't exist
  mkdir -p $streamerNoPort
  [[ -f "$streamerNoPort/stream_stopped" ]] || echo 0 > "$streamerNoPort/stream_stopped"
  [[ -f "$streamerNoPort/last_channel" ]] || echo 0 > "$streamerNoPort/last_channel"

  # Write PID for this script to bmitune_pid for use in stopbmitune.sh
  echo $$ > "$streamerNoPort/bmitune_pid"
  echo "Current PID for this script is $$"
}

#Tuning is based on channel name values from dtvdeeplinks.m3u.
#Resolves the launch activity on-device instead of hardcoding it, force-stops the
#app for a clean state (-S), and waits for the launch to complete (-W).
tuneChannel() {
  local tuneURL="dtvnow://deeplink.directvnow.com/play/channel/$channelName/$channelID"
  local remoteCmd="am start -S -W -n $packageName/\$(cmd package resolve-activity -a android.intent.action.MAIN $packageName | awk -F= '/name=/{print \$2; exit}') -a android.intent.action.VIEW -d '$tuneURL'"

  $adbTarget shell "$remoteCmd"
}

main() {
  updateReferenceFiles
  tuneChannel
}

main
