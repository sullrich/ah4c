#!/bin/bash
# prebmitune.sh for firetv/dtvstreamdeeplinks
# 2026.08.19

#Debug on if uncommented
set -x

streamerIP="$1"
streamerNoPort="${streamerIP%%:*}"
adbTarget="adb -s $streamerIP"
packageName=com.att.tv

mkdir -p $streamerNoPort

#Trap end of script run
finish() {
  echo "prebmitune.sh is exiting for $streamerIP with exit code $?"
}

trap finish EXIT

adbConnect() {
  adb connect $streamerIP

  local -i adbMaxRetries=2
  local -i adbCounter=0

  while true; do
    $adbTarget shell input keyevent KEYCODE_WAKEUP
    local adbEventSuccess=$?

    if [[ $adbEventSuccess -eq 0 ]]; then
      break
    fi

    if (($adbCounter > $adbMaxRetries)); then
      touch $streamerNoPort/adbCommunicationFail
      echo "Communication with $streamerIP failed after $adbMaxRetries retries"
      exit 1
    fi

    sleep 1
    ((adbCounter++))
  done
}

#The app has to be restarted before every tune to be reliable, so it starts
#dead here rather than mid-tune -- bmitune.sh's am start brings it back up
#fresh when it sends the deep link.
forceStopApp() {
  $adbTarget shell am force-stop $packageName
}

main() {
  adbConnect
  forceStopApp
}

main
