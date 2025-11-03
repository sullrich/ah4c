#!/bin/bash
#bmitune.sh for firetv/espn
#2025.11.03

#Debug on if uncommented
set -x

#Global
channelID="$1"
specialID="$1"
streamerIP="$2"
streamerNoPort="${streamerIP%%:*}"
adbTarget="adb -s $streamerIP"
packageName=com.espn.gtv
packageLaunch=com.espn.startup.presentation.StartupActivity

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

#Tuning is based on channel ID values from espn_plus.m3u.
tuneChannel() {
  $adbTarget shell am start -n $packageName/$packageLaunch sportscenter://x-callback-url/showWatchStream?playID=$channelID
}

main() {
  updateReferenceFiles
  tuneChannel
}

main
