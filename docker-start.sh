#!/bin/bash
# docker-start.sh
# 2026.08.24

# Ensure render group can access GPU device
[[ -c /dev/dri/renderD128 ]] && chgrp render /dev/dri/renderD128

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

# List currently connected adb devices, connect to each individually, and make
# wireless debugging persistent on Android 11+ (adb_allowed_connection_time)
adbConnections() {

  local androids=($@)
  adb devices

  for android in "${androids[@]}"
    do
      if [[ -n $android ]]; then
        adb connect $android

        local androidVersion=$(adb -s $android shell getprop ro.build.version.release | tr -d '\r')
        if [[ -n $androidVersion ]] && (( ${androidVersion%%.*} >= 11 )); then
          local adbAllowedTime=$(adb -s $android shell settings get global adb_allowed_connection_time | tr -d '\r')
          if [[ "$adbAllowedTime" == "null" ]]; then
            adb -s $android shell settings put global adb_allowed_connection_time 0
            adbAllowedTime=$(adb -s $android shell settings get global adb_allowed_connection_time | tr -d '\r')
            echo "adb_allowed_connection_time for $android set to $adbAllowedTime"
          fi
        fi
      fi
  done
}

# List currently connected atv devices and then connect to each individually
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

# Create device specific M3Us for use with firetv/livetv channels (adb-based tuners only)
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

# Echo the value of every set variable whose name begins with $1
expandVars() { local v; for v in $(compgen -v "$1"); do echo "${!v}"; done; }

# Fix hostname resolution, connect tuners, copy scripts and M3U files as needed, start ws-scrcpy and ah4c
main() {

  fixTunerDNS $(expandVars TUNER)
  fixEncoderDNS $(expandVars ENCODER)

  if [[ "${PYATV,,}" == "true" ]]; then atvConnections $(expandVars TUNER); else adbConnections $(expandVars TUNER); fi

  checkScripts prebmitune.sh bmitune.sh stopbmitune.sh isconnected.sh keep_alive.sh reboot.sh createm3u.sh common.sh atvpair.sh
  checkM3Us allente.m3u channels.m3u coachella.m3u directv.m3u dtvdeeplinks.m3u dtvosprey.m3u dtvstream.m3u dtvstreamdeeplinks.m3u edc.m3u foo-fighters.m3u fubo.m3u hulu.m3u kodifaves-pbs-seatac.m3u livetv.m3u nbc.m3u npo.m3u pbs-seatac.m3u pbs-worcester.m3u silicondust.m3u sling.m3u spectrum.m3u xfinity.m3u youtubetv_shield.m3u youtubetv.m3u zinwell.m3u

  if [[ "${PYATV,,}" != "true" ]]; then createM3Us $(expandVars TUNER); fi

  [[ -n $USER_SCRIPT ]] && { ./"$USER_SCRIPT" & } || echo "No user-defined custom script to run"
  # Labeled, because its output lands in the same container log as ah4c's and
  # its startup banner reads as somebody else's claim: "Listening on:
  # http://ah4c:8000/" is ws-scrcpy announcing the device-control UI on 8000,
  # printed while ah4c is deliberately not answering on 7654 yet. Unlabeled,
  # that is ah4c appearing to say it is up when it is not.
  npm start --prefix ws-scrcpy 2>&1 | sed -u 's/^/[SCRCPY] /' &
  ./ah4c
}

main
