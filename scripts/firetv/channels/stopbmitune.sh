#!/bin/bash
#stopbmitune.sh for firetv/channels
#2025.05.03

#Debug on if uncommented
set -x

streamerIP="$1"
streamerNoPort="${streamerIP%%:*}"
adbTarget="adb -s $streamerIP"
packageName=com.getchannels.dvr.app
[[ $SPEED_MODE == "" ]] && speedMode="true" || speedMode="$SPEED_MODE"

#Stop stream
adbStop() {
  [[ $speedMode == "true" ]] \
  && stop="input keyevent KEYCODE_BACK" \
  || stop="am force-stop $packageName"

  $adbTarget shell $stop; sleep 2
  echo "Streaming stopped for $streamerIP"
}

#Device sleep
adbSleep() {
  sleep="input keyevent KEYCODE_SLEEP"

  $adbTarget shell $sleep
  echo "Sleep initiated for $streamerIP"
  date +%s > $streamerNoPort/stream_stopped
  echo "$streamerNoPort/stream_stopped written with epoch stop time"
}

main() {
  adbStop
  adbSleep
}

main
