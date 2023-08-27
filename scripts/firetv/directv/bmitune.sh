#!/bin/bash
#bmitune.sh for firetv/directv

#Debug on if uncommented
set -x

#Global
channelID=\""$1\""
specialID="$1"
streamerIP="$2"
streamerNoPort="${streamerIP%%:*}"
adbTarget="adb -s $streamerIP"

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
    *)
        exit 1
        ;;
  esac
}

#Check for active audio stream
activeAudioCheck() {
  local startTime=$(date +%s)
  local maxDuration=50
  local minimumLoudness=-50
  local sleepDuration=1
  
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

    echo "Active audio stream not yet detected -- loudness is $checkLoudness LUF. Continuing..."
    sleep $sleepDuration
  done
}

#Special channels to kill DirecTV app or reboot FireStick
specialChannels() {
    packageName=com.att.tv

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
      appFocus=$($adbTarget shell dumpsys window windows | grep -E 'mCurrentFocus' | cut -d '/' -f1 | sed 's/.* //g')
      echo "Current app in focus is $appFocus" 
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

  # Handle cases where stream_stopped or last_channel don't exist
  mkdir -p $streamerNoPort
  [[ -f "$streamerNoPort/stream_stopped" ]] || echo 0 > "$streamerNoPort/stream_stopped"
  [[ -f "$streamerNoPort/last_channel" ]] || echo 0 > "$streamerNoPort/last_channel"

  # Write PID for this script to bmitune_pid for use in stopbmitune.sh
  echo $$ > "$streamerNoPort/bmitune_pid"
  echo "Current PID for this script is $$"

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

#Tuning is based on channel name values from directv.m3u.
tuneChannel() {
  channelName=$(awk -F, '/channel-id='"$channelID"'/ {print $2}' m3u/directv.m3u)
  channelName=$(echo $channelName | sed 's/^/"/;s/$/"/')
  
  directvMenu="input keyevent KEYCODE_MENU; \
        input keyevent KEYCODE_MENU; \
        input keyevent KEYCODE_MENU; \
        input keyevent KEYCODE_MENU"

  directvSearch="input keyevent KEYCODE_DPAD_RIGHT; \
        input keyevent KEYCODE_DPAD_RIGHT; \
        input keyevent KEYCODE_DPAD_RIGHT; \
        input keyevent KEYCODE_DPAD_RIGHT;
        input keyevent KEYCODE_DPAD_DOWN; sleep 3; \
        input keyevent KEYCODE_DPAD_CENTER; sleep 3"

  directvTune="input keyevent KEYCODE_MEDIA_PLAY_PAUSE; sleep 3; \
        input keyevent KEYCODE_DPAD_DOWN; \
        input keyevent KEYCODE_DPAD_DOWN; \
        input keyevent KEYCODE_DPAD_CENTER"

  $adbTarget shell $directvMenu
  $adbTarget shell $directvSearch
  $adbTarget shell input text $channelName
  $adbTarget shell $directvTune
}

main() {
  matchEncoderURL
  specialChannels
  launchDelay
  tuneChannel
}

main
