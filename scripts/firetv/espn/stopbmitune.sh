#!/bin/bash
#stopbmitune.sh for firetv/espn
#2025.11.03

#Debug on if uncommented
set -x

streamerIP="$1"
streamerNoPort="${streamerIP%%:*}"
adbTarget="adb -s $streamerIP"
packageName="com.espn.gtv"

#Check if bmitune.sh is done running
bmituneDone() {
  bmitunePID=$(<"$streamerNoPort/bmitune_pid")

  while ps -p $bmitunePID > /dev/null; do
    echo "Waiting for bmitune.sh to complete..."
    sleep 2
  done
}

#Stop stream
adbStop() {
  #stop="input keyevent KEYCODE_BACK"
        #input keyevent KEYCODE_BACK; \
        #input keyevent KEYCODE_HOME"
  stop="am force-stop $packageName"
  $adbTarget shell $stop; sleep 4
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
  bmituneDone
  adbStop
  adbSleep
}

main
