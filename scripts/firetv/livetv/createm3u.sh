#! /bin/bash
#createm3u.sh for firetv/livetv

#Debug on if uncommented
#set -x

#Global
streamerIP="$1"
streamerNoPort="${streamerIP%%:*}"
adbTarget="adb -s $streamerIP"
m3uName="$streamerNoPort.m3u"

initializeDevice() {
  redEcho "Waking $streamerNoPort..."

  $adbTarget shell input keyevent KEYCODE_WAKEUP; sleep 2
  $adbTarget shell input keyevent KEYCODE_HOME; sleep 2
  $adbTarget logcat -c; sleep 2
  $adbTarget shell input keyevent KEYCODE_LIVE_TV; sleep 2
  channelID=$($adbTarget shell "input keyevent KEYCODE_LIVE_TV && logcat -d" | grep GuideManager | tail -n 1 | awk -F? '{print$3}')

  while [ -z $channelID ]; do
    channelID=$($adbTarget shell "input keyevent KEYCODE_DPAD_UP; input keyevent KEYCODE_DPAD_DOWN; logcat -d" | grep GuideManager | tail -n 1 | awk -F? '{print$3}')
  done

  startingChannel=$channelID
}

sortM3U() {
  redEcho "Sorting livetv.m3u alphabetically to match FireTV LiveTV Guide..."

  cp m3u/livetv.m3u m3u/$m3uName
  sed -i ':a;N;$!ba;s/\nhttp/ ,http/g' m3u/$m3uName
  [ -z "$SORT_M3US" ] || [ "$SORT_M3US" == "true" ] \
    && sort -t',' -k2 -f -o m3u/$m3uName m3u/$m3uName
  sed -i '/^$/d' m3u/$m3uName
  sed -i 's/ \& / AND /g' m3u/$m3uName
}

updateM3U() {
  redEcho "Reading $m3uName and updating it with device specific channelID..."  
  echo "Starting channelID is $channelID"

  while IFS= read -r currentLineM3U; do
    if [ "$currentLineM3U" != "#EXTM3U" ]; then
      echo "$currentLineM3U" | awk -F, '{print "assigned to M3U channel name: " $2 "\n"}'
      newLineM3U=$(echo "$currentLineM3U" | sed 's|tuner/.*|tuner/'"$channelID"'|')
      sed -i 's|'"$currentLineM3U"'|'"$newLineM3U"'|' m3u/$m3uName
      channelID=$($adbTarget </dev/null shell "input keyevent KEYCODE_DPAD_DOWN && logcat -d" | grep GuideManager | tail -n 1 | awk -F'GuideManager: ' '{print$2}')

      while [ -z "$channelID" ]; do
        channelID=$($adbTarget </dev/null shell "input keyevent KEYCODE_DPAD_UP; input keyevent KEYCODE_DPAD_DOWN; logcat -d" | grep GuideManager | tail -n 1 | awk -F'GuideManager: ' '{print$2}')  
      done

      if [ "$channelID" == "Updating mini details for EPG Ad" ]; then
        redEcho "EPG ad detected, skipping..."
        channelID=$($adbTarget </dev/null shell "input keyevent KEYCODE_DPAD_DOWN && logcat -d" | grep GuideManager | tail -n 1 | awk -F? '{print$3}')
        echo "Current channelID is $channelID"
      else
        channelID=$(echo "$channelID" | awk -F? '{print$3}')

        while [ -z "$channelID" ] || [ "$channelID" == "$previousChannelID" ]; do
          channelID=$($adbTarget </dev/null shell "input keyevent KEYCODE_DPAD_RIGHT; input keyevent KEYCODE_DPAD_LEFT; logcat -d" | grep GuideManager | tail -n 1 | awk -F? '{print$3}')
        done

        echo "Current channelID is $channelID"
        previousChannelID=$channelID
      fi
    fi
  done < "m3u/$m3uName"

  redEcho "Initial pass through the guide completed"
}

checkM3U() {
  redEcho "Beginning second pass to confirm play/tuner values..."
  while IFS= read -r currentLineM3U; do
    if [ "$currentLineM3U" != "#EXTM3U" ]; then
      checkLineM3U=$(echo "$currentLineM3U" | awk -F/ '{print $NF}')
      echo "Confirming a guideID $checkLineM3U is present"
      echo "$currentLineM3U" | awk -F, '{print "assigned to M3U channel name: " $2 "\n"}'
        if [ -z $checkLineM3U ]; then
          newLineM3U=$(echo "$currentLineM3U" | sed 's|tuner/.*|tuner/'"$channelID"'|')
          sed -i 's|'"$currentLineM3U"'|'"$newLineM3U"'|' m3u/$m3uName
          redEcho "Empty M3U play/tuner value found during check. Writing value used for check..."
        fi
      channelID=$($adbTarget </dev/null shell "input keyevent KEYCODE_DPAD_DOWN && logcat -d" | grep GuideManager | tail -n 1 | awk -F'GuideManager: ' '{print$2}')
        if [ "$channelID" == "Updating mini details for EPG Ad" ]; then
          channelID=$($adbTarget </dev/null shell "input keyevent KEYCODE_DPAD_DOWN && logcat -d" | grep GuideManager | tail -n 1 | awk -F? '{print$3}')
        else
          channelID=$(echo "$channelID" | awk -F? '{print$3}')
        fi
    fi
  done < "m3u/$m3uName"

  if [ "$startingChannel" == "$channelID" ]; then
    redEcho "Device specific M3U creation appears successful as the starting and ending channel IDs match."
    redEcho "It's recommended NOT to leave CREATE_M3US set to true. Create new device specific M3Us only as needed."
  else
    redEcho "Device specific M3U creation may have been unsuccessful as the starting and ending channel IDs do not match."
    redEcho "Recheck that your LiveTV channel guide matches your livetv.m3u, hide unwanted sources and channels in the guide."
  fi
}

formatM3U() {
  redEcho "Final format of new $m3uName..."
  sed -i 's/#EXTINF/\n#EXTINF/g' m3u/$m3uName
  sed -i 's/ ,http/\nhttp/g' m3u/$m3uName
  sed -i 's/ AND / \& /g' m3u/$m3uName
}

redEcho() {
  echo -e "\e[31m $1 \e[0m\n"
}

main() {
  initializeDevice
  sortM3U
  updateM3U
  checkM3U
  formatM3U
}

main
