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

echo "$1"  > /tmp/temp.txt
echo "$2" >> /tmp/temp.txt
echo "$3" >> /tmp/temp.txt

STATION="$1"
TUNERIP="$2"
CONTENT_FILE="hulu_contentid.txt"
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

declare -i COUNTER=0
declare -i FAILSAFE=0
declare -i GIVEUP=0
declare -i REBOOTCOUNTER=0
declare -i ADBCOUNTER=0
declare -i MS=0

logger "[STARTING] bmitune.sh $STATION $TUNERIP"

function finish {
	rm -f /tmp/$TUNERIP.lock
	logger "[FINISH] bmitune.sh is ending for $TUNERIP"
}

trap finish EXIT

. ./scripts/firetv/hulu/common_functions.sh

is_ip_address $TUNERIP && adb_connect

setup_provider

# Check if CONTENT_ID is empty
if [ -z "$CONTENT_ID" ]; then
	logger "[ERROR] Invalid option or CONTENT_ID not found in $CONTENT_FILE"
	rm -f /tmp/$TUNERIP.*
	exit 1
fi

# Start Youtube or Hulu
start_provider

# Send media intent URL
tunein

/bin/echo -n "[WAITING] for stream to start..."
while [ "$STATUS" == "notplaying" ]; do
	sleep 1
	if is_media_playing; then
		STATUS="playing"
		/bin/echo ""
		logger "[SUCCESS] Stream $CONTENT_ID has started."
	else
		/bin/echo -n "."
		if ((COUNTER > 30)); then
			((FAILSAFE++))
			/bin/echo ""
			logger "[TIMEOUT] Stream timeout.  Killing PID $PID & Sending intent again ($FAILSAFE) "
			stop_provider
			sleep 1
			start_provider
			sleep 7
			tunein
			COUNTER=0
		fi
		if ((FAILSAFE > 3)); then
			/bin/echo ""
			logger "[ERROR] Could not stream $CONTENT_ID on $TUNERIP."
			/bin/echo -n "[REBOOT] Issuing reboot for $TUNERIP."
			adb -s $TUNERIP shell reboot
			REBOOTCOUNTER=0
			while true; do
				if ((REBOOTCOUNTER > 35)); then
					/bin/echo ""
					break
				fi
				/bin/echo -n "."
				((REBOOTCOUNTER++))
				sleep 3
			done
			adb_connect
			COUNTER=0
			FAILSAFE=0
			((GIVEUP++))
		fi
		if ((GIVEUP > 2)); then
			logger "[FAIL] Cannot stream after rebooting $TUNERIP Android device. Giving up." 
			rm /tmp/$TUNERIP.*
			exit 1
		fi
		((COUNTER++))
	fi
done
