#!/bin/bash
#prebmitune.sh for osprey/dtvospreydeeplinks
# 2026.07.25

#Debug on if uncommented
set -x

streamerIP="$1"
streamerNoPort="${streamerIP%%:*}"
adbTarget="timeout 15 adb -s $streamerIP"
dtvPackage="com.att.tv.openvideo"

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
  $adbTarget shell input keyevent KEYCODE_WAKEUP
  echo "Waking $streamerIP"
  touch $streamerNoPort/adbAppRunning
}

#Block until the app holds audio focus or reports playing, otherwise the tune gets dropped
readinessGate() {
  $adbTarget shell "
    end=\$((SECONDS+10))
    while [ \$SECONDS -lt \$end ]; do
      dumpsys audio 2>/dev/null | grep -qE 'pack: $dtvPackage.*gain: GAIN ' && exit 0
      dumpsys media_session 2>/dev/null | grep -qE 'PlaybackState \{state=3' && exit 0
    done
    exit 1"

  if [[ $? -ne 0 ]]; then
    touch "$streamerNoPort/adbCommunicationFail"
    echo "Readiness gate timed out for $streamerIP -- failing tune so ah4c can try the next tuner"
    exit 2
  fi

  echo "Readiness gate: $streamerIP is live and hot"
}

main() {
  adbConnect
  adbWake
  readinessGate
}

main
