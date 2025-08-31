#!/bin/bash

#set -x

devices=$(adb devices | grep -w "device" | awk '{print $1}')

if [ -z "$devices" ]; then
    echo "No devices connected."
    exit 0
fi

for device in $devices; do
    echo "Device: $device"
    echo "Third-party packages installed:"

    adb -s "$device" shell pm list packages -3 | sed 's/package://g'

    echo -e "\n------------------------------\n"
done
