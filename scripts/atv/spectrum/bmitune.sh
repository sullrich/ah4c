#!/bin/bash
# bmitune.sh for atv/spectrum
# 2026.08.05

channelID="$1"
streamerIP="$2"

/usr/local/bin/atvremote --storage-filename /root/.android/.pyatv.conf -s $streamerIP launch_app=spectrumTV://watch.spectrum.net/livetv/$channelID
