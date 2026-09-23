#!/bin/bash
# bmitune.sh for atv/spectrum
# 2026.09.22

# Channels DVR invocation contract: bmitune.sh <channelId> <atvHost>
#   $1 = channelId (Gracenote/TMS station ID, from tvc-guide-stationid in M3U)
#   $2 = atvHost    (IP of the Apple TV assigned as this tuner)
channelId="$1"
atvHost="$2"

/usr/local/bin/atvremote --storage-filename /root/.android/.pyatv.conf \
  -s "$atvHost" launch_app="spectrumTV://watch.spectrum.net/livetv/${channelId}"
  