#!/bin/bash
#bmitune.sh for osprey/directv

#Debug on if uncommented
set -x

#Global
channelID=\""$1\""
specialID="$1"
streamerIP="$2"
streamerNoPort="${streamerIP%%:*}"
adbTarget="adb -s $streamerIP"
m3uName="${STREAMER_APP#*/*/}.m3u"


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

#Tuning is based on channel name values from $m3uName.
tuneChannel() {
  
  $adbTarget shell input text $channelID; 
}


main() {
  matchEncoderURL
  tuneChannel
}

main
