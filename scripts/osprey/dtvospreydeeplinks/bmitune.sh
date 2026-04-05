#!/bin/bash
# bmitune.sh for osprey/dtvospreydeeplinks
# 2026.04.03
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
  echo -e "#!/bin/bash\n\necho \"[\$(date)] Keep-alive started for $streamerIP (interval: $KEEP_WATCHING)\" > /proc/1/fd/1\nwhile true; do sleep $KEEP_WATCHING; echo \"[\$(date)] Keep-alive sent to $streamerIP\" > /proc/1/fd/1; $adbTarget shell input keyevent KEYCODE_MEDIA_PLAY; done" > ./$streamerNoPort/keep_watching.sh && chmod +x ./$streamerNoPort/keep_watching.sh
  [[ $KEEP_WATCHING ]] && nohup ./$streamerNoPort/keep_watching.sh &
}
main() {
  tuneChannel
}
main
