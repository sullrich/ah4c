#!/bin/bash
#bmitune.sh for allente/livetv
#2024.03.22

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

#Tuning is based on channel number values from allente.m3u
tuneChannel() {
  for (( digit=0; digit<${#channelID}; digit++ )); do
    keypress=${channelID:$digit:1}
    $adbTarget shell input keyevent KEYCODE_$keypress
  done
}

main() {
  tuneChannel
}

main
