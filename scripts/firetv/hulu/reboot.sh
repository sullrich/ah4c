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

function finish {
	#adb -s $TUNERIP disconnect
	date
}

trap finish EXIT

TUNERIP="$1"

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

is_ip_address $ENCODERIP && adb -s $TUNERIP disconnect 
is_ip_address $ENCODERIP && adb connect $TUNERIP

adb -s $TUNERIP reboot

