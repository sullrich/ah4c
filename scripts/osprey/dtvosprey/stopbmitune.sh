#!/bin/bash
# stopbmitune.sh for osprey/dtvosprey
# 2026.07.25

#Debug on if uncommented
set -x

streamerIP="$1"
streamerNoPort="${streamerIP%%:*}"
adbTarget="adb -s $streamerIP"

#Check if bmitune.sh is done running, then kill the heartbeat before the device is slept
bmituneDone() {
  if [[ -f "$streamerNoPort/bmitune_pid" ]]; then
    bmitunePID=$(<"$streamerNoPort/bmitune_pid")

    #Bounded: a recycled PID could otherwise spin this forever and leak the tuner
    local -i waited=0
    while ps -p "$bmitunePID" > /dev/null && (( waited++ < 15 )); do
      echo "Waiting for bmitune.sh to complete..."
      sleep 2
    done
  fi

  if [[ -f "$streamerNoPort/heartbeat_pid" ]]; then
    heartbeatPID=$(<"$streamerNoPort/heartbeat_pid")
    kill -- -"$heartbeatPID" 2>/dev/null
    rm -f "./$streamerNoPort/heartbeat_pid"
  fi
  rm -f "./$streamerNoPort/heartbeat.sh"
}

#Device sleep
adbSleep() {
  sleep="input keyevent KEYCODE_SLEEP"
  $adbTarget shell $sleep
  echo "Sleep initiated for $streamerIP"
  date +%s > "$streamerNoPort/stream_stopped"
  echo "$streamerNoPort/stream_stopped written with epoch stop time"
}

main() {
  bmituneDone
  adbSleep
}

main
