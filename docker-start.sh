#!/bin/bash
# docker-start.sh
# 2025.03.14

#androids=( $TUNER1_IP $TUNER2_IP $TUNER3_IP $TUNER4_IP )
#[[ "$STREAMER_APP" == *"/atv/"* ]] && appleTV=true

# Make tuner hostnames without local domain name resolvable in Alpine containers by adding each to /etc/hosts
fixTunerDNS() {

  local androids=($@)
  local resolvFile=/etc/resolv.conf
  local hostsFile=/etc/hosts
  local localDomain=$(awk '/search/ {print $2}' $resolvFile)
  local ipv4Pattern='^([0-9]{1,3}\.){3}[0-9]{1,3}$'
  local hostnamePattern='^[a-zA-Z0-9_-]+$'
    
  for android in "${androids[@]}"
    do
      local tunerNoPort="${android%%:*}"
      
      if [[ -n $$android ]]; then
        if [[ $tunerNoPort =~ $ipv4Pattern ]]; then
          break
        elif [[ $tunerNoPort =~ $hostnamePattern ]]; then
          tunerIP=$(dig +short $tunerNoPort.$localDomain)
          echo "$tunerIP $tunerNoPort" >> $hostsFile
        fi
      fi
  done
}

# Make encoder hostnames without local domain name resolvable in Alpine containers by adding each to /etc/hosts
fixEncoderDNS() {

  local encoders=($@)
  local resolvFile=/etc/resolv.conf
  local hostsFile=/etc/hosts
  local localDomain=$(awk '/search/ {print $2}' $resolvFile)
  local ipv4Pattern='^([0-9]{1,3}\.){3}[0-9]{1,3}$'
  local hostnamePattern='^[a-zA-Z0-9_-]+$'
      
  for encoder in "${encoders[@]}"
    do
      local encoderNoURL=$(echo "$encoder" | sed -n 's|^.*://\([^/]*\)/.*|\1|p')
            
      if [[ -n $encoder ]]; then
        if [[ $encoderNoURL =~ $ipv4Pattern ]]; then
          break
        elif [[ $encoderNoURL =~ $hostnamePattern ]]; then
          encoderIP=$(dig +short $encoderNoURL.$localDomain)
          echo "$encoderIP $encoderNoURL" >> $hostsFile          
        fi
      fi
  done

  awk '!a[$0]++' $hostsFile
}

# List currently connected adb devices and then connect to each indivdually
adbConnections() {

  local androids=($@)
  adb devices

  for android in "${androids[@]}"
    do
      if [[ -n $android ]]; then
        adb connect $android
      fi
  done
}

# List currently connected atv devices and then connect to each indivdually
atvConnections() {

  local atvs=($@)
  
  for atv in "${atvs[@]}"
    do
      if [[ -n $atv ]]; then
        atvremote --scan-hosts $atv scan
        #atvremote -s $atv --protocol airplay pair
        #atvremote -s $atv --protocol companion pair
        #atvremote -s $atv --protocol raop pair
      fi
  done
}

# Check if a given script is already present in the appropriate scripts directory, and if not, copy it
checkScripts() {

  local scripts=($@)
  mkdir -p ./scripts/firetv/directv ./$STREAMER_APP
  #scripts=( prebmitune.sh bmitune.sh stopbmitune.sh isconnected.sh keep_alive.sh reboot.sh )
  
  for script in "${scripts[@]}"
    do
      if [ ! -f /opt/scripts/firetv/directv/$script ] && [ -f /tmp/scripts/firetv/directv/$script ] || [[ $UPDATE_SCRIPTS == "true" ]]; then
        cp /tmp/scripts/firetv/directv/$script ./scripts/firetv/directv 2>/dev/null \
        && chmod +x ./scripts/firetv/directv/$script \
        && echo "No existing ./scripts/firetv/directv/$script found or UPDATE_SCRIPTS set to true"
      else
        if [ -f /tmp/scripts/firetv/directv/$script ]; then
          echo "Existing ./scripts/firetv/directv/$script found, and will be preserved"
        fi
      fi

      if [ ! -f /opt/$STREAMER_APP/$script ] && [ -f /tmp/$STREAMER_APP/$script ] || [[ $UPDATE_SCRIPTS == "true" ]]; then
        cp /tmp/$STREAMER_APP/$script ./$STREAMER_APP 2>/dev/null \
        && chmod +x ./$STREAMER_APP/$script \
        && echo "No existing ./$STREAMER_APP/$script found or UPDATE_SCRIPTS set to true"
      else
        if [ -f /tmp/$STREAMER_APP/$script ]; then
          echo "Existing ./$STREAMER_APP/$script found, and will be preserved"
        fi
      fi
  done
}

# Check if a given M3U file is already present in the M3U directory, and if not, copy it
checkM3Us() {

  local m3us=($@)
  mkdir -p ./m3u
  #m3us=( directv.m3u foo-fighters.m3u hulu.m3u youtubetv.m3u )

  for m3u in "${m3us[@]}"
    do
      if [ ! -f /opt/m3u/$m3u ] || [[ $UPDATE_M3US == "true" ]]; then
        cp /tmp/m3u/$m3u ./m3u \
        && echo "No existing $m3u found or UPDATE_M3US set to true"
      else
        echo "Existing $m3u found, and will be preserved"
      fi
  done
}

# Create device specific M3Us for use with firetv/livetv channels
createM3Us() {
  local androids=($@)

  for android in "${androids[@]}"
    do
      if [[ -n $android ]] && [[ $CREATE_M3US == "true" ]]; then
        adb -s $android shell input keyevent KEYCODE_WAKEUP; sleep 5
        adb -s $android shell reboot; sleep 45
        $STREAMER_APP/createm3u.sh $android
      fi
  done
}

# Fix hostanme resolution, connect adb devices, copy scripts and M3U files as needed, start ws-scrcpy and ah4c
main() {

  fixTunerDNS $TUNER1_IP $TUNER2_IP $TUNER3_IP $TUNER4_IP $TUNER5_IP $TUNER6_IP $TUNER7_IP $TUNER8_IP $TUNER9_IP
  fixEncoderDNS $ENCODER1_URL $ENCODER2_URL $ENCODER3_URL $ENCODER4_URL $ENCODER5_URL $ENCODER6_URL $ENCODER7_URL $ENCODER8_URL $ENCODER9_URL
  adbConnections $TUNER1_IP $TUNER2_IP $TUNER3_IP $TUNER4_IP $TUNER5_IP $TUNER6_IP $TUNER7_IP $TUNER8_IP $TUNER9_IP
  checkScripts prebmitune.sh bmitune.sh stopbmitune.sh isconnected.sh keep_alive.sh reboot.sh createm3u.sh common.sh
  checkM3Us directv.m3u dtvdeeplinks.m3u dtvosprey.m3u dtvstream.m3u dtvstreamdeeplinks.m3u foo-fighters.m3u fubo.m3u hulu.m3u livetv.m3u nbc.m3u npo.m3u pbs-seatac.m3u pbs-worcester.m3u silicondust.m3u sling.m3u spectrum.m3u xfinity.m3u youtubetv_shield.m3u youtubetv.m3u
  createM3Us $TUNER1_IP $TUNER2_IP $TUNER3_IP $TUNER4_IP $TUNER5_IP $TUNER6_IP $TUNER7_IP $TUNER8_IP $TUNER9_IP
  [[ -n $USER_SCRIPT ]] && { ./"$USER_SCRIPT" & } || echo "No user-defined custom script to run"
  npm start --prefix ws-scrcpy &
  ./ah4c
}

main
