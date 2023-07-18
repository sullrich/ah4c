#!/bin/bash
#CONNECT="connect 192.168.1.171"
WAKE="input keyevent KEYCODE_WAKEUP"
HOME="input keyevent KEYCODE_HOME"

adb connect $1
adb connect $1
adb connect $1
adb -s $1 shell $WAKE
adb -s $1 shell $WAKE
#adb -s $1 shell $WAKE
adb -s $1 shell $HOME; sleep 2
#adb -s $1 shell am start com.google.android.youtube.tvunplugged; sleep 2
#adb -s $1 shell am force-stop com.google.android.youtube.tvunplugged
