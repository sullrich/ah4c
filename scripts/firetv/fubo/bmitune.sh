#!/bin/bash
#bmitune.sh for firetv/fubo

#Debug on if uncommented
set -x

#Global
channelID="$1"
specialID="$1"
streamerIP="$2"
streamerNoPort="${streamerIP%%:*}"
adbTarget="adb -s $streamerIP"
packageName=com.fubo.firetv.screen

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
    *)
        exit 1
        ;;
  esac
}

#Check for active audio stream
activeAudioCheck() {
  local startTime=$(date +%s)
  local maxDuration=60
  local minimumLoudness=-50
  local sleepDuration=0.5
  
  while true; do
    checkLoudness=$(ffmpeg -t 1 -i $encoderURL -filter:a ebur128 -map 0:a -f null -hide_banner - 2>&1 | awk '/I:        /{print $2}')

    if (( $(date +%s) - $startTime > $maxDuration )); then
      echo "Active audio stream not detected in $maxDuration seconds."
      exit 1
    fi

    if (( $(echo "$checkLoudness > $minimumLoudness" | bc -l) )); then
      echo "Active audio stream detected with $checkLoudness LUF."
      break
    fi

    if appFocusCheck 0; then
      echo "Active audio stream not yet detected -- loudness is $checkLoudness LUF. Continuing..."
      sleep $sleepDuration
    else
      echo "No active audio stream detected and app is not in focus after $(($(date +%s) - $startTime)) seconds -- attempting to tune again..."
      #tuneChannel
      $adbTarget shell input keyevent KEYCODE_CENTER
    fi

  done
}

appFocusCheck() {
  appFocus=$($adbTarget shell dumpsys window windows | grep -E 'mCurrentFocus' | cut -d '/' -f1 | sed 's/.* //g')

  if [[ $appFocus == $packageName ]]; then
    return 0
  else
    return 1
  fi
}

#Special channels to kill DirecTV app or reboot FireStick
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

#Tuning is based on channel name values from fubo.m3u.
tuneChannel() {
  #channelName=$(awk '/channel-id='"$channelID"'/ {getline; print}' m3u/fubo.m3u | cut -d'/' -f6)
  #channelName=$(echo $channelName | sed 's/^/"/;s/$/"/')
  
  #livetvMenu="input keyevent KEYCODE_HOME"

  #livetvGuide="input keyevent KEYCODE_LIVE_TV"

  #livetvTune="input keyevent KEYCODE_DPAD_DOWN"

  #$adbTarget shell $livetvMenu
  #$adbTarget shell $livetvGuide
  #$adbTarget shell $livetvGuide
  #$adbTarget shell input text $channelName
  #$adbTarget shell $livetvTune
  #livetvTune
  $adbTarget shell am start -a android.intent.action.VIEW -d https://link.fubo.tv/al1%3Fv%3D1%26a%3Dplay%26t%3Dchannel%26channel_id%3D$channelID
}

main() {
  updateReferenceFiles
  matchEncoderURL
  specialChannels
  #launchDelay
  tuneChannel
  activeAudioCheck
}

main
