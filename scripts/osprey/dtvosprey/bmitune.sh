#!/bin/bash
# bmitune.sh for osprey/dtvosprey
# 2026.07.25
#Debug on if uncommented
set -x
#Global
channelName=$(echo "$1" | awk -F~ '{print $1}')
streamerIP="$2"
streamerNoPort="${streamerIP%%:*}"
adbTarget="timeout 15 adb -s $streamerIP"
dtvPackage="com.att.tv.openvideo"
heartbeatInterval=180
heartbeatKeycode=211

mkdir -p "$streamerNoPort"
echo $$ > "$streamerNoPort/bmitune_pid"

#Trap end of script run
finish() {
  echo "bmitune.sh is exiting for $streamerIP with exit code $?"
}
trap finish EXIT

#Tune by channel number; prebmitune.sh has already woken the box and gated on it being ready for input
tuneChannel() {
  if [[ -z "$channelName" ]]; then
    echo "Tune: ERROR - empty channel name from m3u"
    return 1
  fi

  $adbTarget shell input text "\"$channelName\""
}

#Resets the app's 5 minute UI inactivity timer, which otherwise restarts playback mid stream
#Keycode 211 is inert on a US TV app, unlike media keycodes which draw an OSD
startHeartbeat() {
  if [[ -f "$streamerNoPort/heartbeat_pid" ]]; then
    kill -- -"$(<"$streamerNoPort/heartbeat_pid")" 2>/dev/null
  fi
  cat > "./$streamerNoPort/heartbeat.sh" <<HBEOF
#!/bin/bash
trap 'echo "heartbeat pid killed" > /proc/1/fd/1; exit 0' TERM INT
echo "heartbeat started for $streamerIP -- keyevent $heartbeatKeycode every ${heartbeatInterval}s" > /proc/1/fd/1
while true; do
  sleep $heartbeatInterval
  $adbTarget shell input keyevent $heartbeatKeycode
done
HBEOF
  chmod +x "./$streamerNoPort/heartbeat.sh"
  setsid "./$streamerNoPort/heartbeat.sh" >/dev/null 2>&1 &
  echo $! > "$streamerNoPort/heartbeat_pid"
}

main() {
  tuneChannel || exit 1
  startHeartbeat
}
main
