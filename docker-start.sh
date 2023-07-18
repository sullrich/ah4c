#!/bin/bash

adb devices

androids=( $TUNER1_IP $TUNER2_IP $TUNER3_IP $TUNER4_IP )

for i in "${androids[@]}"
  do
    if [ ! -z $i ]; then
      adb connect $i
    fi
done

mkdir -p ./scripts/onn/youtubetv ./$STREAMER_APP

scripts=( prebmitune.sh bmitune.sh stopbmitune.sh isconnected.sh keep_alive.sh reboot.sh )

for i in "${scripts[@]}"
  do
    if [ ! -f /opt/scripts/onn/youtubetv/$i ] && [ -f /tmp/scripts/onn/youtubetv/$i ]; then
      cp /tmp/scripts/onn/youtubetv/$i ./scripts/onn/youtubetv \
      && chmod +x ./scripts/onn/youtubetv/$i \
      && echo "No existing ./scripts/onn/youtubetv/$i found"
    else
      if [ -f /tmp/scripts/onn/youtubetv/$i ]; then
        echo "Existing ./scripts/onn/youtubetv/$i found, and will be preserved"
      fi
    fi

    if [ ! -f /opt/$STREAMER_APP/$i ] && [ -f /tmp/$STREAMER_APP/$i ]; then
      cp /tmp/$STREAMER_APP/$i ./$STREAMER_APP \
      && chmod +x ./$STREAMER_APP/$i \
      && echo "No existing ./$STREAMER_APP/$i found"
    else
      if [ -f /tmp/$STREAMER_APP/$i ]; then
        echo "Existing ./$STREAMER_APP/$i found, and will be preserved"
      fi
    fi
done

mkdir -p ./m3u

m3us=( directv.m3u foo-fighters.m3u hulu.m3u youtubetv.m3u )

for i in "${m3us[@]}"
  do
    if [ ! -f /opt/m3u/$i ]; then
      cp /tmp/m3u/$i ./m3u \
      && echo "No existing $i found"
    else
      echo "Existing $i found, and will be preserved"
    fi
done

./androidhdmi-for-channels