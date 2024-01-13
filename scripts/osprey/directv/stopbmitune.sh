#!/bin/bash
#stopbmitune.sh for osprey/directv

#Debug on if uncommented
set -x

streamerIP="$1"
streamerNoPort="${streamerIP%%:*}"
adbTarget="adb -s $streamerIP"

#Device sleep
adbSleep() {
  sleep="input keyevent KEYCODE_SLEEP"

  $adbTarget shell $sleep
  echo "Sleep initiated for $streamerIP"
  date +%s > $streamerNoPort/stream_stopped
  echo "$streamerNoPort/stream_stopped written with epoch stop time"
}

main() {
  adbSleep
}

main
