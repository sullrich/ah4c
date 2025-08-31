#!/bin/bash
#stopbmitune.sh for firetv/directv

#Debug on if uncommented
set -x

streamerIP="$1"
streamerNoPort="${streamerIP%%:*}"
adbTarget="adb -s $streamerIP"
packageName=com.att.tv
[[ $SPEED_MODE == "" ]] && speedMode="true" || speedMode="$SPEED_MODE"

#Check if bmitune.sh is done running
bmituneDone() {
  bmitunePID=$(<"$streamerNoPort/bmitune_pid")
  keepWatchingPID=$(pgrep -f "$streamerNoPort/keep_watching.sh")
  keepWatchingPPID=$(ps -o ppid= -p "$keepWatchingPID")
  keepWatchingCPID=$(pgrep -P $keepWatchingPID)

  while ps -p $bmitunePID > /dev/null; do
    echo "Waiting for bmitune.sh to complete..."
    sleep 2
  done

  [[ $KEEP_WATCHING ]] && pkill -P $keepWatchingPPID && kill $keepWatchingCPID
  rm ./$streamerNoPort/keep_watching.sh
}

#Stop stream
adbStop() {
  [[ $speedMode == "true" ]] \
  && stop="input keyevent KEYCODE_BACK; \
          input keyevent KEYCODE_HOME" \
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
  bmituneDone
  adbStop
  adbSleep
}

main
