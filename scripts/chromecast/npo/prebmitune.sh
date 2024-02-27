#!/bin/bash
#prebmitune.sh for chromecast/npo

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

  local -i adbMaxRetries=25
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

adbWake() {
  packageLaunch=".tv.presentation.splash.StartupActivity"
  packageName="nl.uitzendinggemist"
  packagePID=$($adbTarget shell pidof $packageName)
  
  if [ ! -z $packagePID ]; then
    $adbTarget shell input keyevent KEYCODE_WAKEUP
    $adbTarget shell am start -a android.intent.action.VIEW -n $packageName/$packageLaunch
    echo "Waking $streamerIP"
    touch $streamerNoPort/adbAppRunning
  else
    $adbTarget shell input keyevent KEYCODE_WAKEUP
    $adbTarget shell am start -a android.intent.action.VIEW -n $packageName/$packageLaunch
    echo "Starting $packageName on $streamerIP"
  fi
}

main() {
  adbConnect
  adbWake
  sleep 10
}

main
