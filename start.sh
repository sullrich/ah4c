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

IPADDRESS="10.0.250.69"
STREAMER_APP="scripts/firetv/hulu"

NUMBER_TUNERS="2"

ENCODER1_URL="http://10.0.250.110/ts/1_0"
TUNER1_IP="10.0.250.154"
CMD1=""

ENCODER2_URL="http://10.0.250.145/ts/1_0"
TUNER2_IP="10.0.250.158"
CMD2=""

ENCODER3_URL=""
TUNER3_IP=""
CMD3=""

ENCODER4_URL=""
TUNER4_IP=""
CMD4=""

ENCODER5_URL=""
TUNER5_IP=""
CMD5=""

export STREAMER_APP ENCODER1_URL TUNER1_IP ENCODER2_URL TUNER2_IP CMD1 CMD2 CMD3 CMD4 CMD5

if [ -d "m3u/@eaDir" ]; then
	echo ">>> Removing m3u/@eaDir"
	rm -rf "m3u/@eaDir"
fi

if [ -d "html/@eaDir" ]; then
	echo ">>> Removing html/@eaDir"
	rm -rf "html/@eaDir"
fi

go build . && go run .
