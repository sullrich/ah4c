#!/bin/bash
#prebmitune.sh for osprey/dtvospreydeeplinks
# 2026.06.25

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
  $adbTarget shell input keyevent KEYCODE_WAKEUP
  echo "Waking $streamerIP"
  touch $streamerNoPort/adbAppRunning
}

main() {
  adbConnect
  adbWake
  $adbTarget shell 'for i in $(seq 1 80); do dumpsys audio 2>/dev/null | grep -E "pack: com.att.tv.openvideo.*gain: GAIN " >/dev/null && break; dumpsys media_session 2>/dev/null | grep "PlaybackState {state=3" >/dev/null && break; sleep 0.1; done'
}
main
