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

DIR=$(pwd)
echo "Current PWD is $DIR"

if [ -f './env' ]; then
	source ./env
	# Read each line in the file
	while IFS= read -r line; do
		# Check if the line contains a variable assignment
		if [[ $line == *=* ]]; then
			# Export the variable
			varName="${line%%=*}"
			export "$varName"
		fi
	done < ./env
else
	echo "!!! Warning: could not locate ../../../env.  Docker users can ignore this warning."
fi

function is_media_playing() {
	ms=$(adb -s $TUNERIP shell dumpsys media_session | grep  "state=PlaybackState {state=3" | wc -l)
	if ((ms > 0)); then
		return 0
	else
		return 1
	fi
}

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
	WHICH_PROVIDER=$(adb -s $TUNERIP shell pm list package | grep "$1" | grep -v music )
	ISPKG=$(echo "$WHICH_PROVIDER" | grep "package:" | wc -l)
	if [ "$ISPKG" -gt 0 ]; then
		EXE=$(echo "$WHICH_PROVIDER" | cut -d':' -f2)
	else
		EXE=$(echo "$WHICH_PROVIDER")
	fi
	echo $EXE
}

function is_running() {
	RUNNING=$(adb -s $TUNERIP shell ps  | grep "$1" | wc -l)
	if [ "$RUNNING" -gt 0 ]; then
		return 0
	else
		return 1
	fi
}

function start_provider() {
	if [ "$PROVIDER" = "hulu" ]; then
		HULU=$(find_provider hulu)
		echo "Stopping $HULU"
		adb shell monkey -p $HULU 1
		sleep 10
	fi
	if [ "$PROVIDER" = "youtube" ]; then
		YOUTUBE=$(find_provider youtube)
		echo "Stopping $YOUTUBE"
		adb shell monkey -p $YOUTUBE 1
		sleep 10
	fi
}

function stop_provider() {
	HULU=$(find_provider hulu)
	YOUTUBE=$(find_provider youtube)
	is_running hulu && adb -s $TUNERIP shell am force-stop $HULU
	is_running youtube && adb -s $TUNERIP shell am force-stop $YOUTUBE
}

function tunein() {
	if [ "$PROVIDER" = "hulu" ]; then
		echo ">>> Sending media intent for $CONTENT_ID"
		echo adb -s $TUNERIP shell am start -a android.intent.action.VIEW -d "https://www.hulu.com/watch/$CONTENT_ID"
		adb -s $TUNERIP shell am start -a android.intent.action.VIEW -d "https://www.hulu.com/watch/$CONTENT_ID"
	fi
	if [ "$PROVIDER" = "youtube" ]; then
		echo ">>> Sending media intent for $CONTENT_ID"
		echo adb shell am start -a android.intent.action.VIEW -d "https://www.youtube.com/watch?v=$CONTENT_ID&t=1s"
		adb shell am start -a android.intent.action.VIEW -d "https://www.youtube.com/watch?v=$CONTENT_ID&t=1s"
	fi
	if [ "$PROVIDER" = "weatherscan" ]; then
		echo ">>> Sending media intent for $CONTENT_ID"
		echo adb -s $TUNERIP shell am start -a android.intent.action.VIEW -d "$CONTENT_ID"
		adb -s $TUNERIP shell am start -a android.intent.action.VIEW -d "$CONTENT_ID"
		exit
	fi
	if [ "$PROVIDER" = "www" ]; then
		echo ">>> Sending media intent for $CONTENT_ID"
		URL=$(echo "$CONTENT_ID" | tr '\\' '/')
		echo adb -s $TUNERIP shell am start -a android.intent.action.VIEW -d "$URL"
		adb -s $TUNERIP shell am start -a android.intent.action.VIEW -d "$URL"
		exit
	fi
}

function killtunein() {
	HULU=$(find_provider hulu)
	YOUTUBE=$(find_provider youtube)
	is_running hulu && adb -s $TUNERIP shell am force-stop $HULU
	is_running youtube && adb -s $TUNERIP shell am force-stop $YOUTUBE
}

function adb_connect() {
	echo ">>> Connecting ADB to $TUNERIP"
	local -i ADBCOUNTER=0
	while true; do
		ADBSTATUS=$(adb connect $TUNERIP)
		if [[ $ADBSTATUS == *"connected"* ]]; then
			break
		fi
		if [[ $ADBSTATUS == "adb: device offline" ]]; then
			echo "!!! Error with adb"
			rm -f /tmp/$TUNERIP.lock
			exit 1
		fi		
		if ((ADBCOUNTER > 25)); then
			echo "!!! Could not connect via ADB to $TUNERIP"
			rm -f /tmp/$TUNERIP.lock
			exit 1
		fi
		sleep 1
		((ADBCOUNTER++))
	done
}


updatefailcounter() {
	echo "$2" > /tmp/$1.failcounter
}

getfailcounter() {
	failcounter=$(cat /tmp/$1.failcounter)
}

check() {
	IPADDR="$1"
	if [ -f /tmp/$IPADDR.lock ]; then 
		return
	fi
	if [ ! -f /tmp/$IPADDR.playing ]; then
		return
	fi
	if [ $(cat /tmp/$IPADDR.playing) == "weatherscan" ]; then
		return
	fi
	status=$(./isconnected.sh $IPADDR)
	if [ "$status" == "true" ]; then
		failcounter=0
		updatefailcounter $IPADDR $failcounter
		return
	else
		getfailcounter $IPADDR
		((failcounter++))
		updatefailcounter $IPADDR $failcounter
		if ((failcounter > 3)); then
			is_ip_address $IPADDR && adb connect $IPADDR
			adb -s $IPADDR shell dumpsys appops --op TOAST_WINDOW > /tmp/fail.$IPADDR.txt
			failcounter=1
			updatefailcounter $IPADDR $failcounter
			rm /tmp/$IPADDR*.*
			echo "!!! Giving up trying to stream $IPADDR."
			return
		fi	
		station=$(cat /tmp/$IPADDR.playing)
		echo "!!! Performing rescue of $IPADDR $station"
		./bmitune.sh "$station" "$IPADDR"
	fi
}

rebootall(){
	echo "Rebooting devices..."
	for ip in $tunerArray; do 
		adb connect $ip
		adb -s $ip shell reboot
	done
	adb kill-server
	killall adb
	sleep 60
}

keepalive(){
	ip="$1"
	is_ip_address $ip && adb connect $ip
	adb -s $ip shell input keyevent 25
	adb -s $ip shell input keyevent 24
}
