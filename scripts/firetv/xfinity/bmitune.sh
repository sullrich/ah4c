#!/bin/bash
# bmitune.sh for firetv/xfinity
# 2025.03.13

#Debug on if uncommented
set -x

#Global
dvr="$CHANNELSIP:8089"
channelNameID="$1"
channelID=$(echo $1 | awk -F- '{print $2}')
channelName=$(echo $1 | awk -F- '{print $1}')
specialID="$channelName"
streamerIP="$2"
streamerNoPort="${streamerIP%%:*}"
adbTarget="adb -s $streamerIP"
packageName=com.xfinity.cloudtvr.tenfoot
packageAction=com.xfinity.common.view.LaunchActivity
m3uName="${STREAMER_APP#*/*/}.m3u"
m3uChannelID=$(grep -B1 "/play/tuner/$channelNameID" "/opt/m3u/$m3uName" | awk -F 'channel-id="' 'NF>1 {split($2, a, "\""); print a[1]}')
channelNumber=$(curl -s http://$dvr/api/v1/channels | jq -r '.[] | select(.id == "'$m3uChannelID'") | .number')
[[ $SPEED_MODE == "" ]] && speedMode="false" || speedMode="$SPEED_MODE"
read -a autoCropChannels <<< "$AUTOCROP_CHANNELS"

#Trap end of script run
finish() {
  echo "bmitune.sh is exiting for $streamerIP with exit code $?"
}

trap finish EXIT

updateReferenceFiles() {

  # Handle cases where stream_stopped or last_channel don't exist
  mkdir -p $streamerNoPort
  [[ -f "$streamerNoPort/stream_stopped" ]] || echo 0 > "$streamerNoPort/stream_stopped"
  [[ -f "$streamerNoPort/last_channel" ]] || echo 0 > "$streamerNoPort/last_channel"

  # Write PID for this script to bmitune_pid for use in stopbmitune.sh
  echo $$ > "$streamerNoPort/bmitune_pid"
  echo "Current PID for this script is $$"
}

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

#Check for active audio stream with maxDuration, preTuneAudioCheck, sleepBeforeAudioCheck and sleepAfterAudioCheck as arguments
activeAudioCheck() {
  local startTime=$(date +%s)
  local maxDuration=$1
  local minimumLoudness=-50
  local sleepBeforeAudioCheck=$3
  local sleepAfterAudioCheck=$4
  local preTuneAudioCheck=$2
  
  while true; do
    sleep $sleepBeforeAudioCheck
    checkLoudness=$(ffmpeg -t 1 -i $encoderURL -filter:a ebur128 -map 0:a -f null -hide_banner - 2>&1 | awk '/I:        /{print $2}')

    if (( $(date +%s) - $startTime > $maxDuration )); then
      echo "Active audio stream not detected in $maxDuration seconds."
      if [ $preTuneAudioCheck = "false" ]; then
        echo "Active audio stream not detected after tuning completed"
        echo "Killing app and re-tuning..."
        $adbTarget shell am force-stop $packageName; sleep 2
        tuneChannel
        exit 0
      else
        exit 1
      fi
    fi

    if (( $(echo "$checkLoudness > $minimumLoudness" | bc -l) )); then
      echo "Active audio stream detected with $checkLoudness LUF."
      break
    fi

    echo "Active audio stream not yet detected -- loudness is $checkLoudness LUF. Continuing..."
    sleep $sleepAfterAudioCheck
  done
}

tuneCheck() {
  sleep 40
  ffmpeg -i $encoderURL -frames:v 1 -y $streamerNoPort/screencapture.jpg -loglevel quiet
  tesseract $streamerNoPort/screencapture.jpg $streamerNoPort/screencapture
  grep -q "Filter\|Today" $streamerNoPort/screencapture.txt

  if [ $? == 0 ]; then
    echo "Deeplink tuning appears to have failed. Killing app and re-tuning..."
    $adbTarget shell am force-stop $packageName
    tuneChannel
  else
    echo "Deeplink tuning appears to have been successful!"
  fi
}

appFocusCheck() {
  appFocus=$($adbTarget shell dumpsys window windows | grep -E 'mCurrentFocus' | cut -d '/' -f1 | sed 's/.* //g')

  if [[ $appFocus == $packageName ]]; then
    return 0
  else
    return 1
  fi
}

#Special channels to kill app or reboot FireStick
specialChannels() {
    if [ $specialID = "exit" ]; then
      echo "Exit $packageName requested on $streamerIP"
      rm $streamerNoPort/last_channel $streamerNoPort/adbAppRunning
      $adbTarget shell am force-stop $packageName
      exit 0
    elif [ $specialID = "reboot" ]; then
      echo "Reboot $streamerIP requested"
      rm $streamerNoPort/last_channel $streamerNoPort/adbAppRunning
      $adbTarget reboot
      exit 0
    elif [[ -f $streamerNoPort/adbCommunicationFail ]]; then
      rm $streamerNoPort/adbCommunicationFail
      exit 1
    else
      echo "Not a special channel (exit nor reboot)"
      #if appFocusCheck; then
        #echo "$packageName is the app in focus, OK to tune"
      #fi      
    fi
}

#Variable delay based on whether app was running or needed to be launched
#and whether less than maxTime seconds (maxTime/3600 for hours) has passed while sleeping
launchDelay() {
  local lastChannel
  local lastAwake
  local timeNow
  local timeElapsed
  local maxTime=14400

  lastChannel=$(<"$streamerNoPort/last_channel")
  lastAwake=$(<"$streamerNoPort/stream_stopped")
  timeNow=$(date +%s)
  timeElapsed=$(($timeNow - $lastAwake))

  if (( $lastChannel == $specialID )) && (( $timeElapsed < $maxTime )); then
    echo "Last channel selected on this tuner, no channel change required"
    exit 0
  elif [ -f $streamerNoPort/adbAppRunning ] && (( $timeElapsed < $maxTime )); then
    activeAudioCheck
    #sleep 14
    rm $streamerNoPort/adbAppRunning
    echo $specialID > "$streamerNoPort/last_channel"
  else
    activeAudioCheck
    #sleep 32
    echo $specialID > "$streamerNoPort/last_channel"
  fi
}

#Tuning is based on deeplink values from xfinity.m3u.
tuneChannel() {
  $adbTarget shell am start -n $packageName/$packageAction https://www.xfinity.com/stream/live/$channelName/$channelID/$channelName
  echo -e "#!/bin/bash\n\nwhile true; do sleep $KEEP_WATCHING; $adbTarget shell input keyevent KEYCODE_DPAD_DOWN; done" > ./$streamerNoPort/keep_watching.sh && chmod +x ./$streamerNoPort/keep_watching.sh
  [[ $KEEP_WATCHING ]] && nohup ./$streamerNoPort/keep_watching.sh &
}

#For Xfinity channels defined in AUTOCROP_CHANNELS, that have black bars on 4 sides, this function determines crop values for LinkPi Encoders. Standard aspect ratios are used.
determineCrop() {
  sleep 40

  maxRetries=5
  attempt=0
  cropX=0

  while (( cropX == 0 || cropX > 250 )); do
    ((attempt++))
    echo "CropX ($cropX) is currently equal to 0 greater than 250..."

    if (( attempt >= maxRetries )); then
      echo "No sensible crop values found in $maxRetries retries. No autocrop applied."
      return 1
    fi

    sleep 5  # Wait for 5 seconds before rechecking

    # Re-run crop detection
    cropValues=$(ffmpeg -i "$encoderURL" -vf "cropdetect=limit=40:round=2:reset=1" -t 5 -loglevel verbose -f null - 2>&1 | grep -o 'crop=[0-9]*:[0-9]*:[0-9]*:[0-9]*' | tail -n 1)

    # Re-extract crop values
    cropX=$(echo "$cropValues" | sed -n 's/.*crop=[0-9]*:[0-9]*:\([0-9]*\):[0-9]*/\1/p')
  done

  cropWidth=$(echo "$cropValues" | sed -n 's/.*crop=\([0-9]*\):[0-9]*:[0-9]*:[0-9]*/\1/p')
  cropHeight=$(echo "$cropValues" | sed -n 's/.*crop=[0-9]*:\([0-9]*\):[0-9]*:[0-9]*/\1/p')
  #cropX=$(echo "$cropValues" | sed -n 's/.*crop=[0-9]*:[0-9]*:\([0-9]*\):[0-9]*/\1/p')
  cropY=$(echo "$cropValues" | sed -n 's/.*crop=[0-9]*:[0-9]*:[0-9]*:\([0-9]*\)/\1/p')

  # Calculate the detected aspect ratio
  detectedAspectRatio=$(echo "scale=6; $cropWidth / $cropHeight" | bc)

  # List of standard aspect ratios
  declare -A aspectRatios=(
    #["1.33"]="1.3333" # 4:3
    #["1.66"]="1.6667" # European Widesceen
    ["1.78"]="1.7778" # 16:9
    ["1.85"]="1.8500" # Cinema Widescreen
    #["2.00"]="2.0000" # Netflix Originals
    ["2.20"]="2.2000" # 70mm
    ["2.35"]="2.3500" # CinemaScope
    ["2.39"]="2.3900" # Modern Widescreen
  )

  # Find the closest aspect ratio
  closestAspectRatio=""
  minDiff="1000.0"

  for aspectRatio in "${!aspectRatios[@]}"; do
    ratio=${aspectRatios[$aspectRatio]}
    ratioDiff=$(echo "scale=6; ($detectedAspectRatio - $ratio) ^ 2" | bc)
    if (( $(echo "$ratioDiff < $minDiff" | bc -l) )); then
      minDiff=$ratioDiff
      closestAspectRatio=$ratio
    fi
  done

  echo "Detected Aspect Ratio: $detectedAspectRatio"
  echo "Using Closest Standard Aspect Ratio: $closestAspectRatio"

  cropLeft=$(echo "$cropX" | awk '{print int(($1+1)/2)*2}')
  cropRight=$cropLeft
  cropTop=$(echo "scale=6; $cropLeft / $closestAspectRatio" | bc | awk '{print int(($1+1)/2)*2}')
  cropBottom=$cropTop

  echo "Aspect Ratio of Cropped Area: $closestAspectRatio"
  echo "Pixels to Crop from Left: $cropLeft"
  echo "Pixels to Crop from Right: $cropRight"
  echo "Pixels to Crop from Top: $cropTop"
  echo "Pixels to Crop from Bottom: $cropBottom"
}

currentAiring() {
  while true; do
    currentAiringJSON=$(curl -s "http://$dvr/devices/ANY/guide/now?time=$(date +%s)" | 
      jq '[.[] | .Airings[] | select(.Channel == "'$channelNumber'") | 
        {
          "Channel": .Channel,
          "Title": .Title,
          "StartTime": .Time,
          "Duration": .Duration,
          "EndTime": (.Time + .Duration)
        }
      ]'
    )

    currentTime=$(date +%s)
    currentAiringEnd=$(echo "$currentAiringJSON" | jq -r '.[0].EndTime')
    [ "$currentAiringEnd" -gt "$currentTime" ] && sleep $((currentAiringEnd - currentTime)) \
      && linkpiCrop 0 0 0 0
    sleep 90
    [ "$currentAiringEnd" -gt "$currentTime" ] && determineCrop && linkpiCrop $cropLeft $cropRight $cropTop $cropBottom
  done
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

buildCurrentAiring() {
  mkdir -p /opt/$encoderIP

  {
  echo -e "#!/bin/bash\n"
  echo "dvr=$dvr"
  echo "encoderURL=$encoderURL"
  echo -e "channelNumber=$channelNumber\n"
  declare -f currentAiring
  echo
  declare -f determineCrop
  echo
  declare -f linkpiCrop
  echo -e "\ncurrentAiring\n"
  } > "/opt/$encoderIP/$encoderStreamNumber.sh"

  chmod +x "/opt/$encoderIP/$encoderStreamNumber.sh"
  nohup "/opt/$encoderIP/$encoderStreamNumber.sh" &
}

main() {
  updateReferenceFiles
  matchEncoderURL && encoderStreamNumber="${encoderURL##*/}" && encoderIP="${encoderURL#*//}" && encoderIP="${encoderIP%%:*}"
  specialChannels
  #launchDelay
  tuneChannel
  printf "%s\n" "${autoCropChannels[@]}" | grep -qx "$channelNumber" \
    && determineCrop && linkpiCrop $cropLeft $cropRight $cropTop $cropBottom && buildCurrentAiring
  #[[ $speedMode == "true" ]] && activeAudioCheck 40 false 5 1 || [[ $speedMode == "false" ]] # (maxDuration, preTuneAudioCheck, sleepBeforeAudioCheck, sleepAfterAudioCheck)
  #tuneCheck
  :
}

main
