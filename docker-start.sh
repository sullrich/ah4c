#!/bin/bash
# docker-start.sh
# 2026.09.13

# Date stamp of the ah4c.yaml this image was built from. Bump together with the
# AH4C_COMPOSE line in ah4c.yaml whenever the compose file changes shape.
# checkVersions compares it to the AH4C_COMPOSE the running container was
# started with.
LATEST_COMPOSE=2026.09.16

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
        # A device that is asleep, off or moved never answers, and adb waits
        # about two minutes for it, all before ah4c starts serving. Give each
        # device ten seconds; one that misses it is connected when it is used.
        local connected
        connected=$(timeout 10 adb connect $android 2>&1)
        echo "$connected"
        if [[ $connected != *"connected to"* ]]; then
          echo "adb: could not connect to $android within 10 seconds; skipping it at startup"
          continue
        fi

        local androidVersion=$(timeout 10 adb -s $android shell getprop ro.build.version.release | tr -d '\r')
        if [[ -n $androidVersion ]] && (( ${androidVersion%%.*} >= 11 )); then
          local adbAllowedTime=$(timeout 10 adb -s $android shell settings get global adb_allowed_connection_time | tr -d '\r')
          if [[ "$adbAllowedTime" == "null" ]]; then
            timeout 10 adb -s $android shell settings put global adb_allowed_connection_time 0
            adbAllowedTime=$(timeout 10 adb -s $android shell settings get global adb_allowed_connection_time | tr -d '\r')
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
  mkdir -p ./scripts/firetv/directv
  [[ -n "$streamerAppValid" ]] && mkdir -p ./$STREAMER_APP

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

      [[ -n "$streamerAppValid" ]] || continue
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

# scripts/all/all can dispatch a tune to any device/provider (see
# scripts/all/all/bmitune.sh), so unlike checkScripts -- which only ever
# populates the one $STREAMER_APP directory -- it needs every device/provider
# script set on disk, not just the one currently selected. Same copy-if-missing
# rule as checkScripts, just applied to every scripts/<device>/<provider> the
# image was built with instead of one.
checkAllScripts() {

  local scripts=($@)
  local dir rel script

  for dir in /tmp/scripts/*/*/; do
    [[ -d $dir ]] || continue
    rel=${dir#/tmp/}
    rel=${rel%/}
    mkdir -p ./$rel

    for script in "${scripts[@]}"
      do
        if [ ! -f /opt/$rel/$script ] && [ -f /tmp/$rel/$script ] || [[ $UPDATE_SCRIPTS == "true" ]]; then
          cp /tmp/$rel/$script ./$rel 2>/dev/null \
          && chmod +x ./$rel/$script \
          && echo "No existing ./$rel/$script found or UPDATE_SCRIPTS set to true"
        else
          if [ -f /tmp/$rel/$script ]; then
            echo "Existing ./$rel/$script found, and will be preserved"
          fi
        fi
    done
  done
}

# A package may contain any number of helpers, but these are its three entry points.
scriptPackageComplete() {
  local dir="$1" file
  for file in bmitune.sh prebmitune.sh stopbmitune.sh; do
    [ -f "$dir/$file" ] || return 1
  done
}

# Settings may save the folder as ./scripts/x/ or scripts/x; every step below
# wants scripts/x, and none of them may touch a path outside ./scripts.
prepareStreamerApp() {
  local target backup
  STREAMER_APP="${STREAMER_APP#./}"
  while [[ "$STREAMER_APP" == */ ]]; do STREAMER_APP="${STREAMER_APP%/}"; done
  export STREAMER_APP
  streamerAppValid=""
  [[ -n "$STREAMER_APP" ]] || { echo "No STREAMER_APP configured; no tuner scripts requested"; return; }
  [[ "$STREAMER_APP" =~ ^scripts/[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)?$ ]] || { echo "WARNING: Invalid STREAMER_APP path '$STREAMER_APP'"; return; }
  streamerAppValid=true
  # An earlier version swapped whole folders and kept the old one aside while
  # it did. Put that one back only where nothing has taken its place: a folder
  # that exists is the user's, complete or not.
  target="/opt/$STREAMER_APP"
  backup="${target%/*}/.${target##*/}.backup"
  if [ ! -e "$target" ] && scriptPackageComplete "$backup"; then
    mv "$backup" "$target" && echo "Restored the previous $STREAMER_APP package after an interrupted update"
  fi
}

# checkScripts copies what this image carries. A selection it does not carry
# comes from sullrich/ah4c on GitHub under the same rule: a file already here
# is kept unless UPDATE_SCRIPTS is true, and nothing here is ever removed,
# because the folder may hold the user's own edits and helpers.
fetchConfiguredScripts() {
  local work api name url file target
  [[ -n "$streamerAppValid" ]] || return
  [ -d "/tmp/$STREAMER_APP" ] && return
  target="/opt/$STREAMER_APP"
  if scriptPackageComplete "$target" && [[ "${UPDATE_SCRIPTS,,}" != "true" ]]; then
    echo "Existing $STREAMER_APP scripts found; GitHub was not checked"
    return
  fi

  local -a downloads=()
  work=$(mktemp -d /tmp/ah4c-streamer.XXXXXX) || return
  api="https://api.github.com/repos/sullrich/ah4c/contents/$STREAMER_APP?ref=main"
  if ! curl -fsSL --connect-timeout 3 --max-time 8 "$api" -o "$work/files.json"; then
    if scriptPackageComplete "$target"; then
      echo "WARNING: GitHub could not be reached; continuing with the complete local $STREAMER_APP package"
    else
      echo "WARNING: GitHub could not be reached and $STREAMER_APP is not complete locally; it needs bmitune.sh, prebmitune.sh, and stopbmitune.sh"
    fi
    rm -rf "$work"
    return
  fi
  mkdir -p "$work/files"
  while IFS=$'\t' read -r name url; do
    [[ "$name" =~ ^[A-Za-z0-9._-]+$ ]] || continue
    downloads+=(--url "$url" --output "$work/files/$name")
  done < <(jq -r '.[] | select(.type == "file" and .download_url != null) | [.name, .download_url] | @tsv' "$work/files.json")
  if [ ${#downloads[@]} -eq 0 ] || ! curl -fsSL --parallel --parallel-max 20 --connect-timeout 3 --max-time 15 "${downloads[@]}"; then
    echo "WARNING: Could not download the complete $STREAMER_APP package"
    rm -rf "$work"
    return
  fi
  if ! scriptPackageComplete "$work/files"; then
    echo "WARNING: GitHub package $STREAMER_APP must contain bmitune.sh, prebmitune.sh, and stopbmitune.sh; preserving any stored scripts"
    rm -rf "$work"
    return
  fi
  if ! mkdir -p "$target"; then
    echo "WARNING: Could not prepare the local folder for $STREAMER_APP"
    rm -rf "$work"
    return
  fi
  for file in "$work/files"/*; do
    name="${file##*/}"
    if [ -e "$target/$name" ] && [[ "${UPDATE_SCRIPTS,,}" != "true" ]]; then
      echo "Existing ./$STREAMER_APP/$name found, and will be preserved"
      continue
    fi
    cp "$file" "$target/$name" \
    && { [[ "$name" != *.sh ]] || chmod +x "$target/$name"; } \
    && echo "Copied ./$STREAMER_APP/$name from sullrich/ah4c"
  done
  [[ "$STREAMER_APP" == "scripts/all/all" ]] && echo "WARNING: scripts/all/all dispatch targets must already exist under /opt/scripts; ah4c does not bulk-download every provider"
  rm -rf "$work"
}

# Check if a given M3U file is already present in the M3U directory, and if not, copy it
checkM3Us() {

  local m3us=($@)
  mkdir -p ./m3u

  for m3u in "${m3us[@]}"
    do
      if [ ! -f /opt/m3u/$m3u ] || [[ "${UPDATE_M3US:-true}" == "true" ]]; then
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
  if [ -z "${AH4C_COMPOSE}" ]; then
    echo "docker-start.sh: Container template version was not supplied; compatibility check skipped"
  elif [ "${AH4C_COMPOSE}" == "$LATEST_COMPOSE" ]; then
    echo "docker-start.sh: Container template version $AH4C_COMPOSE confirmed as up to date"
  else
    echo "docker-start.sh: WARNING -- container template version '$AH4C_COMPOSE' does not match latest ($LATEST_COMPOSE) -- please update your container settings"
  fi

  # vYYYY.MM.DD.HHMM stamp that bump-version.sh embedded in the binary via //go:embed
  local running
  running=$(grep -aoE 'v20[0-9]{2}\.[0-9]{2}\.[0-9]{2}\.[0-9]{4}' /opt/ah4c | head -n1)
  echo "docker-start.sh: Currently running bnhf/ah4c version ${running:-unknown}"
}

# Fix hostname resolution, connect tuners, copy scripts and M3U files as needed, start ws-scrcpy and ah4c
main() {

  eval "$(./ah4c -print-env)"
  prepareStreamerApp

  fixTunerDNS $(expandVars TUNER)
  fixEncoderDNS $(expandVars ENCODER)

  if [[ "${PYATV,,}" == "true" ]]; then atvConnections $(expandVars TUNER); else adbConnections $(expandVars TUNER); fi

  checkScripts prebmitune.sh bmitune.sh stopbmitune.sh isconnected.sh keep_alive.sh reboot.sh createm3u.sh common.sh atvpair.sh
  [[ "$STREAMER_APP" == "scripts/all/all" ]] && checkAllScripts prebmitune.sh bmitune.sh stopbmitune.sh isconnected.sh keep_alive.sh reboot.sh createm3u.sh common.sh atvpair.sh
  fetchConfiguredScripts || echo "WARNING: Script package preparation failed; continuing ah4c startup"
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
