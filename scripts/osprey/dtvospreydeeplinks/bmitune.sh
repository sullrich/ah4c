#!/bin/bash
# bmitune.sh for osprey/dtvospreydeeplinks
# 2026.07.03
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
  cat > ./$streamerNoPort/keep_watching.sh <<KWEOF
#!/bin/bash
trap 'echo "keep watching pid killed" > /proc/1/fd/1; exit 0' TERM INT
echo "keep watching started interval $KEEP_WATCHING" > /proc/1/fd/1
while true; do
  sleep $KEEP_WATCHING
  echo "keep watching event triggered" > /proc/1/fd/1
  $adbTarget shell input keyevent KEYCODE_MEDIA_PLAY
done
KWEOF
  chmod +x ./$streamerNoPort/keep_watching.sh
  if [[ $KEEP_WATCHING ]]; then
    setsid ./$streamerNoPort/keep_watching.sh >/dev/null 2>&1 &
    echo $! > "$streamerNoPort/keep_watching_pid"
  fi
}
main() {
  tuneChannel
}
main
