#!/bin/bash
#prebmitune.sh for firetv/espn
#2025.11.03

#Debug on if uncommented
set -x

streamerIP="$1"
streamerNoPort="${streamerIP%%:*}"
adbTarget="adb -s $streamerIP"

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

main() {
  adbConnect
}

main