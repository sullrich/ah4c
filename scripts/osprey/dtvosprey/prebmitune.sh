#!/bin/bash
# prebmitune.sh for osprey/dtvosprey
# 2026.09.03

#Debug on if uncommented
set -x

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
  wakePath=$($adbTarget shell '
    if [ "$(getprop ro.build.version.sdk)" = 30 ]; then
      read up _ < /proc/uptime
      service call power 12 i64 "${up%.*}${up#*.}0" i32 2 s16 ah4c s16 com.android.shell >/dev/null 2>&1
      i=0
      while [ $i -lt 20 ]; do
        service call power 16 2>/dev/null | grep -q 00000001 && { echo "through the power manager"; exit 0; }
        i=$((i+1))
      done
    fi
    echo "with KEYCODE_WAKEUP"
    input keyevent KEYCODE_WAKEUP')
}

#Block until the app is playing AND owns the focused window, otherwise input text goes nowhere
#mCurrentFocus is the only signal that says a keystroke will land; mFocusedApp stays set while asleep
readinessGate() {
  $adbTarget shell "
    end=\$((SECONDS+10))
    while [ \$SECONDS -lt \$end ]; do
      if dumpsys audio 2>/dev/null | grep -qE 'pack: $dtvPackage.*gain: GAIN ' || dumpsys media_session 2>/dev/null | grep -qE 'PlaybackState \{state=3'; then
        dumpsys window 2>/dev/null | grep -q 'mCurrentFocus=Window.*$dtvPackage' && exit 0
      fi
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
