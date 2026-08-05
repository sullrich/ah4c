#!/bin/bash
# stopbmitune.sh for atv/spectrum
# 2026.08.05

streamerIP="$1"
channelID="$2"

atvTarget="timeout 15 /usr/local/bin/atvremote --storage-filename /root/.android/.pyatv.conf -s $streamerIP"

# Best-effort: back out of the stream to the home screen
$atvTarget home

# Deterministic backstop: guarantees playback stops and the tuner is
# released even if the app didn't background cleanly above. This is the
# same role KEYCODE_SLEEP plays in the adb-based script sets.
$atvTarget turn_off
