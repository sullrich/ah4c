#!/bin/bash
#stopbmitune.sh for android/yttv
IPADD="$1"

adb -s $IPADD shell input keyevent  86