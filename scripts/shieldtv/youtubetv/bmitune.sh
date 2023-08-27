#!/bin/bash

echo "$1"  > /tmp/temp.txt
echo "$2" >> /tmp/temp.txt
echo "$3" >> /tmp/temp.txt

STATION="$1"
TUNERIP="$2"
CONTENT_FILE="contentid.txt"
CONTENT_ID=""
URL=""
PROVIDER=""
STATUS="notplaying"
ADBSTATUS=""
PID=""
RESULT=""
EXE=""
ISPKG=""
WHICH_PROVIDER=""


content_file="yttv_contentid.txt"
content_id=""

# Read content_id from file
if [ -f "$content_file" ]; then
  content_id=$(grep -w "^$1" "$content_file" | cut -d " " -f3)
fi

# Check if content_id is empty
if [ -z "$content_id" ]; then
  echo "Invalid option or content_id not found in $content_file"
  exit 1
fi

adb -s $TUNERIP shell "am start -a android.intent.action.VIEW -d  https://tv.youtube.com/watch/"$content_id""

adb -s $TUNERIP shell "input keyevent 66"
