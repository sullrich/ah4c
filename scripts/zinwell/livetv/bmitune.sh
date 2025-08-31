#!/bin/bash
# bmitune.sh for zinwell/livetv
# 2025.01.26

#Debug on if uncommented
set -x

#Global
channelID="$1"
streamerIP="$2"
adbTarget="adb -s $streamerIP"

#Trap end of script run
finish() {
  echo "bmitune.sh is exiting for $streamerIP with exit code $?"
}

trap finish EXIT

#Tuning is based on channel number values from zinwell.m3u
tuneChannel() {
  $adbTarget shell input text $channelID
  sleep 1
  $adbTarget shell input keyevent KEYCODE_DPAD_CENTER
}

main() {
  tuneChannel
}

main
