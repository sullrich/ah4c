#!/bin/bash
#prebmitune.sh for osprey/dtvosprey
# 2026.07.25
#Debug on if uncommented
#set -x

streamerIP="$1"
streamerNoPort="${streamerIP%%:*}"
adbTarget="timeout 15 adb -s $streamerIP"
dtvPackage="com.att.tv.openvideo"

mkdir -p "$streamerNoPort"

#Trap end of script run
finish() {
  echo "prebmitune.sh is exiting for $streamerIP with exit code $?"
}

trap finish EXIT

adbConnect() {
  local -i adbMaxRetries=3
  local -i adbCounter=0

  while true; do
    adb connect "$streamerIP"
    $adbTarget shell input keyevent KEYCODE_WAKEUP
    local adbEventSuccess=$?

    if [[ $adbEventSuccess -eq 0 ]]; then
      break
    fi

    if (($adbCounter > $adbMaxRetries)); then
      touch "$streamerNoPort/adbCommunicationFail"
      echo "Communication with $streamerIP failed after $adbMaxRetries retries"
      exit 2
    fi
    ((adbCounter++))
  done
}

adbWake() {
  $adbTarget shell input keyevent KEYCODE_WAKEUP
  echo "Waking $streamerIP"
  touch "$streamerNoPort/adbAppRunning"
}

#Block until the app holds audio focus or reports playing, otherwise the channel digits get dropped
#The host side timeout is the only time authority so the verdict never depends on adb propagating remote exit codes
readyGate() {
  timeout 12 adb -s "$streamerIP" shell "while true; do dumpsys audio 2>/dev/null | grep -qE 'pack: $dtvPackage.*gain: GAIN ' && exit 0; dumpsys media_session 2>/dev/null | grep -qE 'PlaybackState \{state=(3|8)' && exit 0; sleep 0.2; done"
}

main() {
  adbConnect
  adbWake
  if readyGate; then
    echo "Readiness gate: $streamerIP is live and hot"
  else
    touch "$streamerNoPort/adbCommunicationFail"
    echo "Readiness gate timed out for $streamerIP -- failing tune so ah4c can try the next tuner"
    exit 2
  fi
}

main
