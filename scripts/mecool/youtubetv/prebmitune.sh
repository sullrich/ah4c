#!/bin/bash
#prebmitune.sh for android/yttv
STREAMERIP="$1"
ADB_CMD="adb -s $1 shell"
WAKE="input keyevent KEYCODE_WAKEUP"
HOME="input keyevent KEYCODE_HOME"
adb connect $STREAMERIP
$ADB_CMD $HOME