#!/bin/bash
#
# Copyright 2023 Scott Ullrich
# sullrich@gmail.com
#
# Permission is hereby granted, free of charge, to any person obtaining a copy of 
# this software and associated documentation files (the “Software”), to deal in the 
# Software without restriction, including without limitation the rights to use, copy, 
# modify, merge, publish, distribute, sublicense, and/or sell copies of the Software,
# and to permit persons to whom the Software is furnished to do so, subject to the 
# following conditions:
#
# The above copyright notice and this permission notice shall be included in all 
# copies or substantial portions of the Software.
#
# THE SOFTWARE IS PROVIDED “AS IS”, WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED,
# INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A 
# PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT
# HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION 
# OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE 
# SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
#

ENCODERIP="$1"
PROVIDER=$(cat /tmp/$ENCODERIP.provider)
EXE=""

function finish {
	rm -f /tmp/$ENCODERIP.*
}

trap finish EXIT

function is_ip_address() {
    local ip_port=$1
    local ip=${ip_port%:*}  # If a port is included, this removes it

    # If IP address is valid
    if [[ $ip =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]]; then
        IFS='.' read -ra ip_parts <<< "$ip"
        for i in "${ip_parts[@]}"; do
            # If IP address octet is less than 0 or greater than 255
            if ((i < 0 || i > 255)); then
                return 1
            fi
        done
        return 0  # IP address is valid
    else
        return 1  # IP address is invalid
    fi
}

function find_provider() {
	WHICH_PROVIDER=$(adb -s $ENCODERIP shell pm list package | grep "$1" | grep -v music )
	ISPKG=$(echo "$WHICH_PROVIDER" | grep "package:" | wc -l)
	if [ "$ISPKG" -gt 0 ]; then
		EXE=$(echo "$WHICH_PROVIDER" | cut -d':' -f2)
	else
		EXE=$(echo "$WHICH_PROVIDER")
	fi
	echo $EXE
}

function is_running() {
	RUNNING=$(adb -s $ENCODERIP shell ps  | grep "$1" | awk '{ print $9 }' | wc -l)
	if [ "$RUNNING" -gt 0 ]; then
		return 1
	else
		return 0
	fi
}

is_ip_address $ENCODERIP && adb connect $ENCODERIP

HULU=$(find_provider hulu)
YOUTUBE=$(find_provider youtube)

is_running hulu && adb -s $ENCODERIP shell am force-stop $HULU
is_running youtube && adb -s $ENCODERIP shell am force-stop $YOUTUBE

adb -s $ENCODERIP shell input keyevent KEYCODE_HOME

if [ "$PROVIDER" = "hulu" ]; then
	echo "Starting hulu $HULU"
	adb -s $ENCODERIP shell monkey -p $HULU -c android.intent.category.LAUNCHER 1
	echo adb -s $ENCODERIP shell monkey -p $HULU -c android.intent.category.LAUNCHER 1
fi

if [ "$PROVIDER" = "youtube" ]; then
	echo "Starting youtube $YOUTUBE"
	echo adb -s $ENCODERIP shell monkey -p $YOUTUBE -c android.intent.category.LAUNCHER 1
	adb -s $ENCODERIP shell monkey -p $YOUTUBE -c android.intent.category.LAUNCHER 1
fi

