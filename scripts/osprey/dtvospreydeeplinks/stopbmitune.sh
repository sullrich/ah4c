#!/bin/bash
# stopbmitune.sh for osprey/dtvospreydeeplinks
# 2026.04.03
#Debug on if uncommented
set -x

streamerIP="$1"
streamerNoPort="${streamerIP%%:*}"
adbTarget="adb -s $streamerIP"

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
  adbSleep
}
main
