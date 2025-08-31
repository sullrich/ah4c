#!/bin/bash
#bmitune.sh for firetv/dtvdeeplinks
#2024.09.18

#Debug on if uncommented
set -x

#Global
channelID=$(echo $1 | awk -F~ '{print $2}')
channelName=$(echo $1 | awk -F~ '{print $1}')
specialID="$channelName"
streamerIP="$2"
streamerNoPort="${streamerIP%%:*}"
adbTarget="adb -s $streamerIP"
packageName=com.att.tv
packageAction=com.clientapp.MainActivity
[[ $SPEED_MODE == "" ]] && speedMode="true" || speedMode="$SPEED_MODE"

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
        case "$specialID" in
          "212")
            echo "Possible sports event blackout on NFL Network, so bumping channel up"
            $adbTarget shell input keyevent KEYCODE_DPAD_LEFT
            echo 0 > "$streamerNoPort/last_channel"
            exit 1
            ;; 
          "213")
            echo "Possible sports event blackout on MLB Network, so bumping channel down"
            $adbTarget shell input keyevent KEYCODE_DPAD_RIGHT
            echo 0 > "$streamerNoPort/last_channel"
            exit 1
            ;; 
          *)
            echo "Possible sports event blackout, so bumping channel down"
            $adbTarget shell input keyevent KEYCODE_DPAD_RIGHT
            echo 0 > "$streamerNoPort/last_channel"
            exit 1
            ;;
        esac
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

appFocusCheck() {
  appFocus=$($adbTarget shell dumpsys window windows | grep -E 'mCurrentFocus' | cut -d '/' -f1 | sed 's/.* //g')

  if [[ $appFocus == $packageName ]]; then
    return 0
  else
    return 1
  fi
}

tuneCheck() {
  local sleepBeforeTuneCheck=$1

  sleep $sleepBeforeTuneCheck
  ffmpeg -i $encoderURL -frames:v 1 -y $streamerNoPort/screencapture.jpg -loglevel quiet
  tesseract $streamerNoPort/screencapture.jpg $streamerNoPort/screencapture
  grep -q "Guide\|Your\|TV" $streamerNoPort/screencapture.txt

  if [ $? == 0 ]; then
    echo "Deeplink tuning appears to have failed. Killing app and re-tuning..."
    $adbTarget shell am force-stop $packageName
    tuneChannel
  else
    echo "Deeplink tuning appears to have been successful!"
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
# launchDelay() {
#   local lastChannel
#   local lastAwake
#   local timeNow
#   local timeElapsed
#   local maxTime=14400

#   lastChannel=$(<"$streamerNoPort/last_channel")
#   lastAwake=$(<"$streamerNoPort/stream_stopped")
#   timeNow=$(date +%s)
#   timeElapsed=$(($timeNow - $lastAwake))

#   if (( $lastChannel == $specialID )) && (( $timeElapsed < $maxTime )); then
#     echo "Last channel selected on this tuner, no channel change required"
#     exit 0
#   elif [ -f $streamerNoPort/adbAppRunning ] && (( $timeElapsed < $maxTime )); then
#     activeAudioCheck
#     #sleep 14
#     rm $streamerNoPort/adbAppRunning
#     echo $specialID > "$streamerNoPort/last_channel"
#   else
#     activeAudioCheck
#     #sleep 32
#     echo $specialID > "$streamerNoPort/last_channel"
#   fi
# }

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

  # if (( $lastChannel == $specialID )) && (( $timeElapsed < $maxTime )); then
  #   echo "Last channel selected on this tuner, no channel change required"
  #   exit 0
  if [ -f $streamerNoPort/adbAppRunning ] && (( $timeElapsed < $maxTime )); then
    rm $streamerNoPort/adbAppRunning
    echo $specialID > "$streamerNoPort/last_channel"
  else
    activeAudioCheck 42 true 0 1 # (maxDuration, preTuneAudioCheck, sleepBeforeAudioCheck, sleepAfterAudioCheck)
    echo $specialID > "$streamerNoPort/last_channel"
  fi
}

#Tuning is based on channel name values from dtvdeeplinks.m3u.
tuneChannel() {
  #$adbTarget shell am start -a android.intent.action.VIEW -d dtvnow://deeplink.directvnow.com/play/channel/$channelName/$channelID
  $adbTarget shell am start -n $packageName/$packageAction dtvnow://deeplink.directvnow.com/play/channel/$channelName/$channelID
  echo -e "#!/bin/bash\n\nwhile true; do sleep $KEEP_WATCHING; $adbTarget shell am start -n $packageName/$packageAction dtvnow://deeplink.directvnow.com/play/channel/$channelName/$channelID; done" > ./$streamerNoPort/keep_watching.sh && chmod +x ./$streamerNoPort/keep_watching.sh
  [[ $KEEP_WATCHING ]] && nohup ./$streamerNoPort/keep_watching.sh &
}

main() {
  updateReferenceFiles
  matchEncoderURL
  specialChannels
  launchDelay
  tuneChannel
  #[[ $speedMode == "true" ]] && tuneCheck 24 # (sleepBeforeTuneCheck)
}

main
