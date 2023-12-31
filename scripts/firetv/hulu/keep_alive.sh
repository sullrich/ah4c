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

STATION=""
TUNERIP=""
PROVIDER=""
CONTENT_ID=""

function finish {
	date
	echo "keep_alive.sh is exiting."
}

trap finish EXIT

declare -i counter=0
declare -i failcounter=0
declare -i i=0
declare -i numTuners=0

# Array to hold the encoders
declare -a tunerArray
declare -a tunerDeviceArray
declare -a tunerURLArray

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

# Get the number of tuners
numTuners=$NUMBER_TUNERS

. ./scripts/firetv/hulu/common_functions.sh

# Loop through the tuner environment variables
for i in $(seq 1 $numTuners); do
	varName="TUNER${i}_IP"
	cmdvarName="CMD${i}_DEVICE"
	encoderVarName="ENCODER${i}_URL"
	TUNERIP=${!varName}
	CMDDEVICE=${!cmdvarName}
	ENCODERURL=${!encoderVarName}
	tuneripArray+=($TUNERIP)
	tunerDeviceArray+=($CMDDEVICE)
	tunerURLArray+=($ENCODERURL)
done

if [ "$i" -lt 1 ]; then
	echo "Could not find ENV variables describing tuners."
	exit 1
fi

while [ /bin/true ]; do
	echo ""
	date
	for index in ${!tuneripArray[@]}; do 
		ip=${tuneripArray[$index]}
		device=${tunerDeviceArray[$index]}
		encoderurl=${tunerURLArray[$index]}
		if ((counter > 3600)); then
			keepalive $ip $device
			counter=0
		fi
		if [ "$encoderurl" != "" ]; then 
			check $ip $encoderurl
		fi
		if [ "$device" != "" ]; then 
			check $ip $device
		fi
		((counter++))
		TIMEA=$(/bin/date "+%H:%M")
		if [ "$TIMEA" = "05:00" ]; then
			rebootall
		fi
	done
	sleep 5
done


