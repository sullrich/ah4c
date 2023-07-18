#!/bin/bash
#prebmitune.sh for firetv/directv

#Debug on if uncommented
set -x

streamerIP="$1"
adbTarget="adb -s $streamerIP"

adb connect $streamerIP

adbWake() {
  packageLaunch="tv.youi.clientapp.AppActivity"
  packageName="com.att.tv"
  packagePID=$($adbTarget shell pidof $packageName)
  
  if [ ! -z $packagePID ]; then
    $adbTarget shell input keyevent KEYCODE_WAKEUP
    $adbTarget shell am start -n $packageName/$packageLaunch
    echo "Waking $streamerIP"
    touch /tmp/wake
  else
    $adbTarget shell input keyevent KEYCODE_WAKEUP
    $adbTarget shell am start -n $packageName/$packageLaunch
    echo "Starting $packageName on $streamerIP"
  fi
}

adbWake
