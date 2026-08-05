#!/bin/bash
# prebmitune.sh for atv/spectrum
# 2026.08.05

streamerIP="$1"

atvTarget="timeout 15 /usr/local/bin/atvremote --storage-filename /root/.android/.pyatv.conf -s $streamerIP"

# Wake the Apple TV -- also wakes a device suspended by stopbmitune.sh's turn_off
$atvTarget home
sleep 2

# Gracefully open the Spectrum app to its default startup screen
$atvTarget launch_app=spectrumTV://
sleep 7
