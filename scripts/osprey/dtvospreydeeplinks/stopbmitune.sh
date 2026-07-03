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

  while ps -p $bmitunePID > /dev/null; do
    echo "Waiting for bmitune.sh to complete..."
    sleep 2
  done

  if [[ $KEEP_WATCHING && -f "$streamerNoPort/keep_watching_pid" ]]; then
    keepWatchingPID=$(<"$streamerNoPort/keep_watching_pid")
    kill -- -"$keepWatchingPID" 2>/dev/null
    rm -f "./$streamerNoPort/keep_watching_pid"
  fi
  rm -f "./$streamerNoPort/keep_watching.sh"
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
