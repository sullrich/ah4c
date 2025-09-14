#!/bin/bash
#prebmitune.sh for osprey/dtvosprey
# 2025.09.10

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

  local -i adbMaxRetries=3
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
      exit 2
    fi
    

    ((adbCounter++))
  done
}

adbWake() {

    $adbTarget shell input keyevent KEYCODE_WAKEUP; sleep 2;
    echo "Waking $streamerIP"
    touch $streamerNoPort/adbAppRunning

}

main() {
  adbConnect
  adbWake
}

main
