#!/bin/bash
# docker-start.sh
# 2026.09.03

# Date stamp of the ah4c.yaml this image was built from. Bump together with the
# AH4C_COMPOSE line in ah4c.yaml whenever the compose file changes shape.
# checkVersions compares it to the AH4C_COMPOSE the running container was
# started with.
LATEST_COMPOSE=2026.09.03

# Fold the container startup output into ah4c's own log file so the WebUI Logs
# page shows one log, not just ah4c's lines. fd 3 keeps the real stdout for
# `docker logs`; every other line this script, its helpers and ws-scrcpy print
# is timestamped to match ah4c's format and appended to /tmp/ah4c.log as well.
# ah4c is exec'd on fd 3 at the end - it writes /tmp/ah4c.log itself, so routing
# it through the tee would double every line.
exec 3>&1
exec > >(while IFS= read -r line; do printf '%(%Y/%m/%d %H:%M:%S)T %s\n' -1 "$line"; done | tee -a /tmp/ah4c.log) 2>&1

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

# Confirm the compose file is current and report the running image version,
# mirroring bnhf/apcupsd-master-slave. Its output is timestamped and captured to
# /tmp/ah4c.log by the redirect at the top of this script, so it shows in the
# WebUI Logs page too. Informational only - nothing is blocked.
checkVersions() {
  if [ "${AH4C_COMPOSE}" == "$LATEST_COMPOSE" ]; then
    echo "docker-start.sh: Docker Compose version $AH4C_COMPOSE confirmed as up to date"
  else
    echo "docker-start.sh: WARNING -- Docker Compose version '${AH4C_COMPOSE:-unset}' does not match latest ($LATEST_COMPOSE) -- please update your compose file"
  fi

  # vYYYY.MM.DD.HHMM stamp that bump-version.sh embedded in the binary via //go:embed
  local running
  running=$(grep -aoE 'v20[0-9]{2}\.[0-9]{2}\.[0-9]{2}\.[0-9]{4}' /opt/ah4c | head -n1)
  echo "docker-start.sh: Currently running bnhf/ah4c version ${running:-unknown}"
}

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
  # Print the version summary once ah4c is actually serving, so it lands at the
  # end of the startup log rather than buried in the middle of ah4c's own
  # output. Capped so it still prints if the port never comes up.
  ( n=0; until curl -sf -o /dev/null --max-time 2 http://localhost:7654/api/version || [ $n -ge 180 ]; do n=$((n + 1)); sleep 1; done; checkVersions ) &
  # On fd 3 (the real stdout), not the tee: ah4c writes /tmp/ah4c.log itself.
  exec ./ah4c >&3 2>&3
}

main
