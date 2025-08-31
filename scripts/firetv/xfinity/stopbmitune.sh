#!/bin/bash
# stopbmitune.sh for firetv/xfinity
# 2025.03.14

#Debug on if uncommented
set -x

dvr="$CHANNELSIP:8089"
streamerIP="$1"
streamerNoPort="${streamerIP%%:*}"
channelNameID="$2"
adbTarget="adb -s $streamerIP"
packageName="com.xfinity.cloudtvr.tenfoot"
m3uName="${STREAMER_APP#*/*/}.m3u"
m3uChannelID=$(grep -B1 "/play/tuner/$channelNameID" "/opt/m3u/$m3uName" | awk -F 'channel-id="' 'NF>1 {split($2, a, "\""); print a[1]}')
channelNumber=$(curl -s http://$dvr/api/v1/channels | jq -r '.[] | select(.id == "'$m3uChannelID'") | .number')
[[ $SPEED_MODE == "" ]] && speedMode="false" || speedMode="$SPEED_MODE"
read -a autoCropChannels <<< "$AUTOCROP_CHANNELS"
printf "%s\n" "${autoCropChannels[@]}" | grep -qx "$channelNumber" && croppedChannel="true"

#Set encoderURL based on the value of streamerIP
matchEncoderURL() {

  case "$streamerIP" in
    "$TUNER1_IP") encoderURL=$ENCODER1_URL ;;
    "$TUNER2_IP") encoderURL=$ENCODER2_URL ;;
    "$TUNER3_IP") encoderURL=$ENCODER3_URL ;;
    "$TUNER4_IP") encoderURL=$ENCODER4_URL ;;
    "$TUNER5_IP") encoderURL=$ENCODER5_URL ;;
    "$TUNER6_IP") encoderURL=$ENCODER6_URL ;;
    "$TUNER7_IP") encoderURL=$ENCODER7_URL ;;
    "$TUNER8_IP") encoderURL=$ENCODER8_URL ;;
    "$TUNER9_IP") encoderURL=$ENCODER9_URL ;;
    *) exit 1 ;;
  esac
}

linkpiCrop() {
  linkpiStreamID=$(echo "$encoderURL" | awk -F'stream' '{print $2}')
  linkpiCropLeft="\"$1\""
  linkpiCropRight="\"$2\""
  linkpiCropTop="\"$3\""
  linkpiCropBottom="\"$4\""
  linkpiPasswordMD5=$(echo -n admin | md5sum | awk '{print $1}')

  linkpiJSON=$(curl -v -c linkpi_cookie --digest -u "$LINKPI_USERNAME:admin" "http://$LINKPI_HOSTNAME/link/user/lph_login?username=$LINKPI_USERNAME&passwd=$linkpiPasswordMD5")
  lHash=$(echo "$linkpiJSON" | jq -r '.data."L-HASH"')
  pHash=$(echo "$linkpiJSON" | jq -r '.data."P-HASH"')
  hHash=$(echo "$linkpiJSON" | jq -r '.data."H-HASH"')

  # echo "L-HASH: $lHash"
  # echo "P-HASH: $pHash"
  # echo "H-HASH: $hHash"

  curl -s --digest -u "$LINKPI_USERNAME:admin" -b linkpi_cookie -X POST -L http://$LINKPI_HOSTNAME/link/encoder/set_cap_chns \
      -H "L-HASH: $lHash" \
      -H "P-HASH: $pHash" \
      -H "H-HASH: $hHash" \
      -H "Content-Type: application/json" \
      -d '[{"id":'$linkpiStreamID', "L":'$linkpiCropLeft', "R":'$linkpiCropRight', "T":'$linkpiCropTop', "B":'$linkpiCropBottom'}]'

  curl -s --digest -u "$LINKPI_USERNAME:admin" -b linkpi_cookie -L http://$LINKPI_HOSTNAME/link/user/lph_logout \
      -H "L-HASH: $lHash" \
      -H "P-HASH: $pHash" \
      -H "H-HASH: $hHash"
}

#Check if bmitune.sh is done running
bmituneDone() {
  bmitunePID=$(<"$streamerNoPort/bmitune_pid")
  keepWatchingPID=$(pgrep -f "$streamerNoPort/keep_watching.sh")
  #keepWatchingPPID=$(ps -o ppid= -p "$keepWatchingPID")
  keepWatchingCPID=$(pgrep -P $keepWatchingPID)

  if [[ $croppedChannel ]]; then
    currentAiringPID=$(pgrep -f "$encoderIP/$encoderStreamNumber.sh")
    #currentAiringPPID=$(ps -o ppid= -p "$currentAiringPID")
    currentAiringCPID=$(pgrep -P $currentAiringPID)
  fi

  while ps -p $bmitunePID > /dev/null; do
    echo "Waiting for bmitune.sh to complete..."
    sleep 2
  done

  #[[ $KEEP_WATCHING ]] && kill $keepWatchingPID && pkill -P $keepWatchingCPID
  [[ $KEEP_WATCHING ]] && kill $keepWatchingPID $keepWatchingCPID
  rm ./$streamerNoPort/keep_watching.sh
  #[[ $croppedChannel ]] && linkpiCrop 0 0 0 0 && kill $currentAiringPID && pkill -P $currentAiringCPID
  [[ $croppedChannel ]] && linkpiCrop 0 0 0 0 && kill $currentAiringPID $currentAiringCPID
  rm ./$encoderIP/$encoderStreamNumber.sh
}

#Stop stream
adbStop() {
  [[ $speedMode == "true" ]] \
  && stop="input keyevent KEYCODE_BACK; \
          input keyevent KEYCODE_HOME" \
  || stop="am force-stop $packageName"
  $adbTarget shell $stop; sleep 2
  echo "Streaming stopped for $streamerIP"
}

#Device sleep
adbSleep() {
  sleep="input keyevent KEYCODE_SLEEP"

  $adbTarget shell $sleep
  echo "Sleep initiated for $streamerIP"
  date +%s > $streamerNoPort/stream_stopped
  echo "$streamerNoPort/stream_stopped written with epoch stop time"
}

main() {
  [[ croppedChannel ]] && matchEncoderURL && encoderStreamNumber="${encoderURL##*/}" \
    && encoderIP="${encoderURL#*//}" && encoderIP="${encoderIP%%:*}"
  bmituneDone
  adbStop
  adbSleep
}

main
