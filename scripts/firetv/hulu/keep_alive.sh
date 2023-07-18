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

if [ -f '../../../env' ]; then
	source ../../../env
	# Read each line in the file
	while IFS= read -r line; do
		# Check if the line contains a variable assignment
		if [[ $line == *=* ]]; then
			# Export the variable
			varName="${line%%=*}"
			export "$varName"
		fi
	done < ../../../env
fi

function finish {
	date
	echo "keep_alive.sh is exiting."
}

trap finish EXIT

declare -i counter=0
declare -i failcounter=0
declare -i i=0

# Array to hold the encoders
declare -a tunerArray

# Get the number of tuners
numTuners=$NUMBER_TUNERS

# Loop through the tuner environment variables
for i in $(seq 1 $numTuners); do
	# Use indirect variable reference to access the environment variable
	varName="TUNER${i}_IP"
	TUNERIP=${!varName}
	# Add the IP address to the array
	tunerArray+=($TUNERIP)
done

if [ "$i" -lt 1 ]; then
	echo "Could not find ENV variables describing tuners."
	exit 1
fi

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
			#adb -s $IPADDR disconnect
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
	killall adb
	sleep 60
}

keepalive(){
	ip="$1"
	is_ip_address $ip && adb connect $ip
	adb -s $ip shell input keyevent 25
	adb -s $ip shell input keyevent 24
}

while [ /bin/true ]; do
	echo ""
	date
	for ip in $tunerArray; do 
		if ((counter > 60)); then
			keepalive $ip
			counter=0
		fi
		check $ip
		((counter++))
		TIMEA=$(/bin/date "+%H:%M")
		if [ "$TIMEA" = "05:00" ]; then
			rebootall
		fi
	done
	sleep 15
done

