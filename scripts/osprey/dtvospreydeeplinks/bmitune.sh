#!/bin/bash
# bmitune.sh for osprey/dtvospreydeeplinks
# 2026.07.25
#Debug on if uncommented
set -x
#Global
channelID=$(echo $1 | awk -F~ '{print $2}')
channelName=$(echo $1 | awk -F~ '{print $1}')
specialID="$channelName"
streamerIP="$2"
streamerNoPort="${streamerIP%%:*}"
adbTarget="adb -s $streamerIP"
[[ $SPEED_MODE == "" ]] && speedMode="true" || speedMode="$SPEED_MODE"
heartbeatInterval=180
heartbeatKeycode=KEYCODE_ZENKAKU_HANKAKU

mkdir -p $streamerNoPort
echo $$ > "$streamerNoPort/bmitune_pid"

#Trap end of script run
finish() {
  echo "bmitune.sh is exiting for $streamerIP with exit code $?"
}
trap finish EXIT
#Set encoderURL based on the value of streamerIP
matchEncoderURL() {
  case "$streamerIP" in
    "$TUNER1_IP")
        encoderURL=$ENCODER1_URL
        ;;
    "$TUNER2_IP")
        encoderURL=$ENCODER2_URL
        ;;
    "$TUNER3_IP")
        encoderURL=$ENCODER3_URL
        ;;
    "$TUNER4_IP")
        encoderURL=$ENCODER4_URL
        ;;
    "$TUNER5_IP")
        encoderURL=$ENCODER5_URL
        ;;
    "$TUNER6_IP")
        encoderURL=$ENCODER6_URL
        ;;
    "$TUNER7_IP")
        encoderURL=$ENCODER7_URL
        ;;
    "$TUNER8_IP")
        encoderURL=$ENCODER8_URL
        ;;
    "$TUNER9_IP")
        encoderURL=$ENCODER9_URL
        ;;
    *)
        exit 1
        ;;
  esac
}
#Tuning is based on channel name/ID values from dtvospreydeeplinks.m3u.
tuneChannel() {
  $adbTarget shell "am start -a android.intent.action.VIEW -d 'https://deeplink.directvnow.com/tune/live/channel/$channelName/$channelID' com.att.tv.openvideo"
}
#Resets the app's 5 minute UI inactivity timer, which otherwise restarts playback mid stream
#KEYCODE_ZENKAKU_HANKAKU is inert on a US TV app, unlike media keycodes which draw an OSD
startHeartbeat() {
  if [[ -f "$streamerNoPort/heartbeat_pid" ]]; then
    kill -- -"$(<"$streamerNoPort/heartbeat_pid")" 2>/dev/null
  fi
  cat > ./$streamerNoPort/heartbeat.sh <<HBEOF
#!/bin/bash
trap 'echo "heartbeat pid killed" > /proc/1/fd/1; exit 0' TERM INT
echo "heartbeat started for $streamerIP -- keyevent $heartbeatKeycode every ${heartbeatInterval}s" > /proc/1/fd/1
while true; do
  sleep $heartbeatInterval
  $adbTarget shell input keyevent $heartbeatKeycode
done
HBEOF
  chmod +x ./$streamerNoPort/heartbeat.sh
  setsid ./$streamerNoPort/heartbeat.sh >/dev/null 2>&1 &
  echo $! > "$streamerNoPort/heartbeat_pid"
}
main() {
  tuneChannel
  startHeartbeat
}
main
