#!/bin/bash
STOP="am force-stop com.google.android.youtube.tvunplugged; sleep 2"

#Stop Video
adb -s $1 shell $STOP
adb -s $1 shell input keyevent KEYCODE_SLEEP
