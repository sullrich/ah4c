#!/bin/bash
#bmitune.sh for android/yttv
ADB_CMD="adb -s $2 shell"
CHANNEL=\""$1\""
APP_LAUNCH="com.google.android.youtube.tvunplugged"
APP_NAME="com.google.android.apps.youtube.tvunplugged.activity.MainActivity"

#Send the command
$ADB_CMD am start -a android.intent.action.VIEW -d https://tv.youtube.com/watch/$CHANNEL -n $APP_LAUNCH/$APP_NAME