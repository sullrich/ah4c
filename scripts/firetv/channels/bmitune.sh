#!/bin/bash
#bmitune.sh for firetv/channels
#2025.05.03

#Debug on if uncommented
set -x

#Global
channelID="$1"
streamerIP="$2"
streamerNoPort="${streamerIP%%:*}"
adbTarget="adb -s $streamerIP"
packageName=com.getchannels.dvr.app
packageAction=com.getchannels.android.MainActivity
[[ $SPEED_MODE == "" ]] && speedMode="true" || speedMode="$SPEED_MODE"

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

appFocusCheck() {
  appFocus=$($adbTarget shell dumpsys window windows | grep -E 'mCurrentFocus' | cut -d '/' -f1 | sed 's/.* //g')

  if [[ $appFocus == $packageName ]]; then
    return 0
  else
    return 1
  fi
}

#Tuning is based on channel name values from channels.m3u.
tuneChannel() {
  ! appFocusCheck && $adbTarget shell am start -n $packageName/$packageAction && sleep 3
  curl -s -X POST http://$streamerNoPort:57000/api/play/channel/$channelID
}

main() {
  updateReferenceFiles
  tuneChannel
}

main
