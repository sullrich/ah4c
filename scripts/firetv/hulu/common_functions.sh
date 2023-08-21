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

function logger() {
	DATE=$(date +"%Y-%d-%m %T")
	echo "$DATE $@"
}

DIR=$(pwd)
logger "[PWD] Current PWD is $DIR"

if [ -f './env' ]; then
	source ./env
	# Read each line in the file
	while IFS= read -r line; do
		# Check if the line contains a variable assignment
		if [[ $line == *=* ]]; then
			# Extract the variable name and value
			varName="${line%%=*}"
			varValue="${line#*=}"
			# Remove the quotes around the variable value if any
			varValue=$(echo $varValue | tr -d '"')
			# Export the variable
			export "$varName=$varValue"
		fi
	done < ./env
else
	logger "[WARNING] Could not locate ./env.  Docker users can ignore this warning."
fi

function setup_provider() {
	echo "$STATION" > /tmp/$TUNERIP.playing
	if [ $(echo "$STATION" | grep "youtube__" | wc -l) -gt 0 ]; then
	    CONTENT_ID=$(echo "$STATION" | awk -F '__' '{print $2}')
	    PROVIDER="youtube"
	elif [ $(echo "$STATION" | grep "hulu__" | wc -l) -gt 0 ]; then
	    CONTENT_ID=$(echo "$STATION" | awk -F '__' '{print $2}')
	    PROVIDER="hulu"
	elif [ $(echo "$STATION" | grep "www" | wc -l) -gt 0 ]; then
	    CONTENT_ID=$(echo "$STATION" | awk -F '__' '{print $2}')
	    PROVIDER="www"
	elif [ $(echo "$STATION" | grep "weatherscan" | wc -l) -gt 0 ]; then
	    CONTENT_ID="http://weatherscan.net"
	    PROVIDER="weatherscan"
	elif [ $(echo "$STATION" | grep "silicondust" | wc -l) -gt 0 ]; then
	    CONTENT_ID=$(echo "$STATION" | awk -F '__' '{print $2}')
	    PROVIDER="silicondust"
	elif [ $(echo "$STATION" | grep "tunein" | wc -l) -gt 0 ]; then
	    # Simply tune into device and do not run any apps
	    exit 0
	else
	    PROVIDER="hulu"
	    if [ -f "$CONTENT_FILE" ]; then
			CONTENT_ID=$(echo "$STATION" | awk -F '__' '{print $2}')
	    fi    
	fi
	echo "$PROVIDER" > /tmp/$TUNERIP.provider
	echo "$CONTENT_ID" > /tmp/$TUNERIP.contentid
}

check() {
	IPADDR="$1"
	DEVICEORURL="$2"
	if [ -f /tmp/$IPADDR.lock ]; then 
		return
	fi
	if [ ! -f /tmp/$IPADDR.playing ]; then
		return
	fi
	if [ $(cat /tmp/$IPADDR.playing) == "weatherscan" ]; then
		return
	fi
	if [ $(cat /tmp/$IPADDR.playing) == "www" ]; then
		return
	fi
	station=$(cat /tmp/$IPADDR.playing)	
	if is_media_frozen $DEVICEORURL $IPADDR $station; then
		failcounter=0
		updatefailcounter $IPADDR $failcounter
		return
	else
		logger "[ERROR] Performing pixel lockup rescue of $IPADDR $station"
		./$STREAMER_APP/bmitune.sh "$station" "$IPADDR"
		return
	fi
	if is_media_playing; then
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
			rm /tmp/$IPADDR.*
			logger "[FAIL] Giving up trying to stream $IPADDR."
			return
		fi	
		logger "[ERROR] Performing rescue of $IPADDR $station"
		./$STREAMER_APP/bmitune.sh "$station" "$IPADDR"
	fi
}

function check_vid_md5() {
	IPADDR=$1
	PLATFORM=$(uname)
	if [ "$PLATFORM" == "Linux" ]; then
		CMD5=$(md5sum /tmp/$IPADDR-current.jpg | awk '{ print $1 }')
		PMD5=$(md5sum /tmp/$IPADDR-previous.jpg | awk '{ print $1 }')
	fi
	if [ "$PLATFORM" == "Darwin" ]; then
		CMD5=$(md5 /tmp/$IPADDR-current.jpg | awk '{ print $4 }')
		PMD5=$(md5 /tmp/$IPADDR-previous.jpg | awk '{ print $4 }')
	fi
	if [ "$CMD5" == "$PMD5" ]; then
		return 1
	fi
	return 0
}

function is_media_playing() {
	RESULT=$(adb -s $TUNERIP shell dumpsys media_session)
	if [ "$PROVIDER" = "silicondust" ]; then
		ms=$(echo "$RESULT" | grep "packages=com.silicondust.view" | wc -l)
		if ((ms > 0)); then
			return 0
		else
			return 1
		fi	
	fi
	ms=$(echo "$RESULT" | grep  "state=PlaybackState {state=3" | wc -l)
	if ((ms > 0)); then
		return 0
	else
		return 1
	fi
}

function is_media_frozen() {
	DEVICEORURL=$1
	IPADDR=$2
	STATION=$3
	if [ "$DEVICEORURL" != "" ]; then
		rm -f "/tmp/$IPADDR-current.jpg"
		if [ ! -f /tmp/$IPADDR-previous.jpg ]; then
			ffmpeg -i "$DEVICEORURL" -frames:v 1 -y "/tmp/$IPADDR-previous.jpg" -loglevel quiet
			sleep 5 
		fi
		ffmpeg -i "$DEVICEORURL" -frames:v 1 -y "/tmp/$IPADDR-current.jpg" -loglevel quiet
		check_vid_md5 $IPADDR
		if [ $? = 1 ]; then
			# screen is frozen. attempt restart
			STATION=$(cat /tmp/$IPADDR.playing)
			TUNERIP="$IPADDR"
			PROVIDER=$(cat /tmp/$IPADDR.provider)
			CONTENT_ID=$(cat /tmp/$IPADDR.contentid)
			logger "[ERROR] Detected pixel lockup ($DEVICEORURL).  Sending intent $STATION."
			return 1
		fi
		rm -f "/tmp/$IPADDR-previous.jpg"
		ffmpeg -i "$DEVICEORURL" -frames:v 1 -y "/tmp/$IPADDR-previous.jpg" -loglevel quiet
	fi
	return 0
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
		logger "[STARTING] $HULU"
		adb -s $TUNERIP shell monkey -p $HULU 1
		sleep 10
	fi
	if [ "$PROVIDER" = "youtube" ]; then
		YOUTUBE=$(find_provider youtube)
		logger "[STARTING] $YOUTUBE"
		adb -s $TUNERIP shell monkey -p $YOUTUBE 1
		sleep 10
	fi
	if [ "$PROVIDER" = "silicondust" ]; then
		SILICONDUST=$(find_provider silicondust)
		logger "[STARTING] $SILICONDUST"
		adb -s $TUNERIP shell monkey -p $SILICONDUST 1
	fi
}

function stop_provider() {
	HULU=$(find_provider hulu)
	YOUTUBE=$(find_provider youtube)
	SILICONDUST=$(find_provider silicondust)
	is_running hulu && adb -s $TUNERIP shell am force-stop $HULU
	is_running youtube && adb -s $TUNERIP shell am force-stop $YOUTUBE
	is_running silicondust && adb -s $TUNERIP shell am force-stop $SILICONDUST
}

function tunein_silicondust() {
	sleep 2
	logger "[TUNEIN] Navigating to channel $CONTENT_ID"
	adb -s $TUNERIP shell input keyevent KEYCODE_DPAD_CENTER
	adb -s $TUNERIP shell input keyevent KEYCODE_DPAD_RIGHT
	adb -s $TUNERIP shell input keyevent KEYCODE_DPAD_RIGHT
	adb -s $TUNERIP shell input keyevent KEYCODE_DPAD_DOWN
	CONTENT=$CONTENT_ID
	for ((i=0; i<${#CONTENT}; i++)); do
		char="${CONTENT:$i:1}"
		if [ "$char" = "!" ]; then
			logger "Sending KEYCODE_DPAD_UP"
			adb -s $TUNERIP shell input keyevent KEYCODE_DPAD_UP
		else
			logger "Sending $char"
			adb -s $TUNERIP shell input text $char		
		fi
	done
	adb -s $TUNERIP shell input keyevent KEYCODE_ENTER
}

function tunein() {
	if [ "$PROVIDER" = "hulu" ]; then
		logger "[TUNEIN] Sending media intent for $CONTENT_ID"
		logger adb -s $TUNERIP shell am start -a android.intent.action.VIEW -d "https://www.hulu.com/watch/$CONTENT_ID"
		adb -s $TUNERIP shell am start -a android.intent.action.VIEW -d "https://www.hulu.com/watch/$CONTENT_ID"
	fi
	if [ "$PROVIDER" = "youtube" ]; then
		logger "[TUNEIN] Sending media intent for $CONTENT_ID"
		logger adb -s $TUNERIP shell am start -a android.intent.action.VIEW -d "https://www.youtube.com/watch?v=$CONTENT_ID&t=1s"
		adb -s $TUNERIP shell am start -a android.intent.action.VIEW -d "https://www.youtube.com/watch?v=$CONTENT_ID&t=1s"
	fi
	if [ "$PROVIDER" = "silicondust" ]; then
		logger "[TUNEIN] Sending media intent for $CONTENT_ID"
		logger adb -s $TUNERIP am start -W -n com.silicondust.view/.App
		adb -s $TUNERIP am start -W -n com.silicondust.view/.App
		tunein_silicondust $CONTENT_ID
	fi
	if [ "$PROVIDER" = "weatherscan" ]; then
		logger "[TUNEIN] Sending media intent for $CONTENT_ID"
		logger adb -s $TUNERIP shell am start -a android.intent.action.VIEW -d "$CONTENT_ID"
		adb -s $TUNERIP shell am start -a android.intent.action.VIEW -d "$CONTENT_ID"
		exit 0
	fi
	if [ "$PROVIDER" = "www" ]; then
		logger "[TUNEIN] Sending media intent for $CONTENT_ID"
		URL=$(echo "$CONTENT_ID" | tr '\\' '/')
		logger adb -s $TUNERIP shell am start -a android.intent.action.VIEW -d "$URL"
		adb -s $TUNERIP shell am start -a android.intent.action.VIEW -d "$URL"
		exit 0
	fi
}

function adb_connect() {
	logger "[CONNECTING] ADB to $TUNERIP"
	local -i ADBCOUNTER=0
	while true; do
		ADBSTATUS=$(adb connect $TUNERIP)
		if [[ $ADBSTATUS == *"connected"* ]]; then
			break
		fi
		if [[ $ADBSTATUS == "adb: device offline" ]]; then
			logger "!!! Error with adb"
			rm -f /tmp/$TUNERIP.lock
			exit 1
		fi		
		if ((ADBCOUNTER > 25)); then
			logger "!!! Could not connect via ADB to $TUNERIP"
			rm -f /tmp/$TUNERIP.lock
			exit 1
		fi
		sleep 1
		((ADBCOUNTER++))
	done
}

updatefailcounter() {
	if [ ! -f /tmp/$1.failcounter ]; then
		echo 1 > /tmp/$1.failcounter
	else
		echo "$2" > /tmp/$1.failcounter
	fi
}

getfailcounter() {
	if [ ! -f /tmp/$1.failcounter ]; then 
		echo 1 > /tmp/$1.failcounter
		failcounter=1
	else
		failcounter=$(cat /tmp/$1.failcounter)
	fi
}

rebootall(){
	logger "[INFO] Rebooting devices..."
	for ip in $tunerArray; do 
		adb connect "$ip"
		logger "[INFO] Rebooting $ip"
		adb -s "$ip" shell reboot
	done
	adb kill-server
	killall adb
	sleep 60
}

keepalive(){
	ip="$1"
	is_ip_address "$ip" && adb connect $ip
	adb -s "$ip" shell input keyevent 25
	adb -s "$ip" shell input keyevent 24
	adb -s "$ip" shell input keyevent 24
}
