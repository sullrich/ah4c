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

echo "$1" > /tmp/temp.txt
echo "$2" >> /tmp/temp.txt
echo "$3" >> /tmp/temp.txt

STATION="$1"
TUNERIP="$2"
content_file="hulu_contentid.txt"
content_id=""
URL=""
PROVIDER=""
status="notplaying"

declare -i counter=0
declare -i failsafe=0
declare -i giveup=0

function finish {
	date
	rm -f /tmp/$TUNERIP.lock
	echo "bmitune.sh is ending for $TUNERIP"
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

date
echo "bmitune.sh is starting for $STATION $TUNERIP"
echo "$STATION" > /tmp/$TUNERIP.playing

function rmlock() {
	rm -f /tmp/$TUNERIP.lock
}

function finish {
	rmlock
}

function tunein() {
	if [ "$PROVIDER" = "hulu" ]; then
		echo ">>> Sending media intent for $content_id"
		echo adb -s $TUNERIP shell am start -a android.intent.action.VIEW -d "https://www.hulu.com/watch/$content_id"
		adb -s $TUNERIP shell am start -a android.intent.action.VIEW -d "https://www.hulu.com/watch/$content_id"
	fi
	if [ "$PROVIDER" = "youtube" ]; then
		echo ">>> Sending media intent for $content_id"
		adb shell am start -a android.intent.action.VIEW -d "https://www.youtube.com/watch?v=$content_id&t=1s"
	fi
	if [ "$PROVIDER" = "weatherscan" ]; then
		adb -s $TUNERIP shell am start -a android.intent.action.VIEW -d "$content_id"
		exit
	fi
	if [ "$PROVIDER" = "www" ]; then
		URL=$(echo "$content_id" | tr '\\' '/')
		adb -s $TUNERIP shell am start -a android.intent.action.VIEW -d "$URL"
		exit
	fi
}

trap finish EXIT

function adb_connect() {
	echo ">>> Connecting ADB to $TUNERIP"
	local -i adbcounter=0
	while true; do
		adbstatus=$(adb connect $TUNERIP)
		if [[ $adbstatus == *"connected"* ]]; then
			break
		fi
		if [[ $adbstatus == "adb: device offline" ]]; then
			echo "!!! Error with adb"
			rm -f /tmp/$TUNERIP.lock
			exit 1
		fi		
		if ((adbcounter > 25)); then
			echo "!!! Could not connect via ADB to $TUNERIP"
			rm -f /tmp/$TUNERIP.lock
			exit 1
		fi
		sleep 1
		((adbcounter++))
	done
}

is_ip_address $TUNERIP && adb_connect

content_id=""

if [ $(echo "$STATION" | grep "youtube__" | wc -l) -gt 0 ]; then
    content_id=$(echo "$STATION" | awk -F '__' '{print $2}')
    PROVIDER="youtube"
elif [ $(echo "$STATION" | grep "hulu__" | wc -l) -gt 0 ]; then
    content_id=$(echo "$STATION" | awk -F '__' '{print $2}')
    PROVIDER="hulu"
elif [ $(echo "$STATION" | grep "www" | wc -l) -gt 0 ]; then
    content_id=$(echo "$STATION" | awk -F '__' '{print $2}')
    PROVIDER="www"
elif [ $(echo "$STATION" | grep "weatherscan" | wc -l) -gt 0 ]; then
    content_id="http://weatherscan.net"
    PROVIDER="weatherscan"
elif [ $(echo "$STATION" | grep "tunein" | wc -l) -gt 0 ]; then
    # Simply tune into device and do not run any apps
    exit 0
else
    PROVIDER="hulu"
    if [ -f "$content_file" ]; then
		content_id=$(echo "$STATION" | awk -F '__' '{print $2}')
    fi    
fi

echo "$PROVIDER" > /tmp/$TUNERIP.provider
echo "$content_id" > /tmp/$TUNERIP.contentid

# Check if content_id is empty
if [ -z "$content_id" ]; then
	echo "Invalid option or content_id not found in $content_file"
	rm -f /tmp/$TUNERIP.lock
	exit 1
fi

tunein

/bin/echo -n ">>> Waiting for stream to start..."
while [ "$status" == "notplaying" ]; do
	sleep 1
	ms=$(adb -s $TUNERIP shell dumpsys media_session | grep  "state=PlaybackState {state=3" | wc -l)
	if ((ms > 0)); then
		status="playing"
		echo ""
		echo ">>> Stream $content_id has started."
	else
		/bin/echo -n "."
		if ((counter > 30)); then
			((failsafe++))
			PID=$(adb -s $TUNERIP shell ps | grep hulu | awk '{ print $2 }')
			/bin/echo ""
			/bin/echo ">>> Stream timeout.  Killing PID $PID & Sending intent again ($failsafe) "
			RESULT=$(adb -s $TUNERIP kill $PID)
			sleep 1
			tunein
			counter=0
		fi
		if ((failsafe > 3)); then
			/bin/echo ""
			/bin/echo "!!! Could not stream $content_id on $TUNERIP."
			/bin/echo -n "!!! Issuing reboot for $TUNERIP."
			adb -s $TUNERIP shell reboot
			declare -i rebootcounter=0
			while true; do
				if ((rebootcounter > 35)); then
					echo
					break
				fi
				/bin/echo -n "."
				((rebootcounter++))
				sleep 3
			done
			adb_connect
			declare -i counter=0
			declare -i failsafe=0
			((giveup++))
		fi
		if ((giveup > 2)); then
			/bin/echo -n ">>> Cannot stream after rebooting $TUNERIP Android device. Giving up." 
			rm -f /tmp/$TUNERIP.lock
			exit 1
		fi
		((counter++))
	fi
done
