#!/bin/bash
#stopbmitune.sh for firetv/directv

#Debug on if uncommented
set -x

streamerIP="$1"
adbTarget="adb -s $streamerIP"

#Stop stream
adbStop() {
  stop="input keyevent KEYCODE_HOME"

  $adbTarget shell $stop; sleep 2
  echo "Streaming stopped for $streamerIP"
}

#Device sleep
adbSleep() {
  sleep="input keyevent KEYCODE_SLEEP"
  streamerNoPort="$(echo "$streamerIP" | awk -F: '{print $1}')"

  $adbTarget shell $sleep
  echo "Sleep initiated for $streamerIP"
  date +%s > /tmp/$streamerNoPort/stream_stopped
  echo "/tmp/$streamerNoPort/stream_stopped written with epoch stop time"
}

main() {
  adbStop
  adbSleep
}

main
