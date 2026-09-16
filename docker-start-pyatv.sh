#!/bin/bash
# docker-start-pyatv.sh
# 2026.09.03

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

#androids=( $TUNER1_IP $TUNER2_IP $TUNER3_IP $TUNER4_IP )

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

# A package may contain any number of helpers, but these are its three entry points.
scriptPackageComplete() {
  local dir="$1" file
  for file in bmitune.sh prebmitune.sh stopbmitune.sh; do
    [ -f "$dir/$file" ] || return 1
  done
}

# Download only the configured streamer directory, never the repository's full scripts tree.
fetchConfiguredScripts() {
  local work api name url packageTarget packageParent packageName stage backup
  [[ -n "$STREAMER_APP" ]] || { echo "No STREAMER_APP configured; no tuner scripts requested"; return; }
  [[ "$STREAMER_APP" =~ ^scripts/[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$ ]] || { echo "WARNING: Invalid STREAMER_APP path '$STREAMER_APP'"; return; }
  packageTarget="/opt/$STREAMER_APP"
  packageParent="${packageTarget%/*}"
  packageName="${packageTarget##*/}"
  backup="$packageParent/.${packageName}.backup"
  if ! scriptPackageComplete "$packageTarget" && scriptPackageComplete "$backup"; then
    [ -e "$packageTarget" ] && rm -rf "$packageTarget"
    mv "$backup" "$packageTarget" && echo "Restored the previous $STREAMER_APP package after an interrupted update"
  elif scriptPackageComplete "$packageTarget" && [ -e "$backup" ]; then
    rm -rf "$backup"
  fi
  if scriptPackageComplete "/opt/$STREAMER_APP" && [[ "${UPDATE_SCRIPTS,,}" != "true" ]]; then
    echo "Existing $STREAMER_APP scripts found; GitHub was not checked"
    return
  fi

  local -a downloads=()
  work=$(mktemp -d /tmp/ah4c-streamer.XXXXXX) || return
  api="https://api.github.com/repos/sullrich/ah4c/contents/$STREAMER_APP?ref=main"
  if ! curl -fsSL --connect-timeout 3 --max-time 8 "$api" -o "$work/files.json"; then
    if scriptPackageComplete "/opt/$STREAMER_APP"; then
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
  if ! mkdir -p "$packageParent"; then
    echo "WARNING: Could not prepare the local folder for $STREAMER_APP"
    rm -rf "$work"
    return
  fi
  stage=$(mktemp -d "$packageParent/.${packageName}.update.XXXXXX") || { rm -rf "$work"; return; }
  if ! cp -a "$work/files/." "$stage/"; then
    echo "WARNING: Could not stage the updated $STREAMER_APP package"
    rm -rf "$stage" "$work"
    return
  fi
  find "$stage" -maxdepth 1 -name '*.sh' -exec chmod +x {} +
  [ -e "$backup" ] && rm -rf "$backup"
  if [ -e "$packageTarget" ] && ! mv "$packageTarget" "$backup"; then
    echo "WARNING: Could not preserve the existing $STREAMER_APP package"
    rm -rf "$stage" "$work"
    return
  fi
  if ! mv "$stage" "$packageTarget"; then
    echo "WARNING: Could not activate the updated $STREAMER_APP package; restoring the existing package"
    [ -e "$backup" ] && mv "$backup" "$packageTarget"
    rm -rf "$stage" "$work"
    return
  fi
  [ -e "$backup" ] && rm -rf "$backup"
  echo "Downloaded only the configured $STREAMER_APP directory from sullrich/ah4c"
  [[ "$STREAMER_APP" == "scripts/all/all" ]] && echo "WARNING: scripts/all/all dispatch targets must already exist under /opt/scripts; ah4c does not bulk-download every provider"
  rm -rf "$work"
}

expandVars() { local v; for v in $(compgen -v "$1"); do echo "${!v}"; done; }

# Check if a given M3U file is already present in the M3U directory, and if not, copy it
checkM3Us() {

  local m3us=($@)
  mkdir -p ./m3u
  #m3us=( directv.m3u foo-fighters.m3u hulu.m3u youtubetv.m3u )

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

# Confirm the compose file is current and report the running image version,
# mirroring bnhf/apcupsd-master-slave. Its output is timestamped and captured to
# /tmp/ah4c.log by the redirect at the top of this script, so it shows in the
# WebUI Logs page too. Informational only - nothing is blocked.
checkVersions() {
  if [ "${AH4C_COMPOSE}" == "$LATEST_COMPOSE" ]; then
    echo "docker-start-pyatv.sh: Docker Compose version $AH4C_COMPOSE confirmed as up to date"
  else
    echo "docker-start-pyatv.sh: WARNING -- Docker Compose version '${AH4C_COMPOSE:-unset}' does not match latest ($LATEST_COMPOSE) -- please update your compose file"
  fi

  # vYYYY.MM.DD.HHMM stamp that bump-version.sh embedded in the binary via //go:embed
  local running
  running=$(grep -aoE 'v20[0-9]{2}\.[0-9]{2}\.[0-9]{2}\.[0-9]{4}' /opt/ah4c | head -n1)
  echo "docker-start-pyatv.sh: Currently running bnhf/ah4c version ${running:-unknown}"
}

# Fix hostanme resolution, connect adb devices, copy scripts and M3U files as needed, start ws-scrcpy and ah4c
main() {

  eval "$(./ah4c -print-env)"
  fetchConfiguredScripts || echo "WARNING: Script package preparation failed; continuing ah4c startup"

  fixTunerDNS $(expandVars TUNER)
  fixEncoderDNS $(expandVars ENCODER)
  atvConnections $(expandVars TUNER)
  checkM3Us directv.m3u dtvosprey.m3u dtvstream.m3u foo-fighters.m3u fubo.m3u hulu.m3u livetv.m3u npo.m3u silicondust.m3u sling.m3u spectrum.m3u youtubetv_shield.m3u youtubetv.m3u
  #createM3Us $TUNER1_IP $TUNER2_IP $TUNER3_IP $TUNER4_IP
  [[ -n $USER_SCRIPT ]] && { ./"$USER_SCRIPT" & } || echo "No user-defined custom script to run"
  # Print the version summary once ah4c is actually serving, so it lands at the
  # end of the startup log rather than buried in the middle of ah4c's own
  # output. Capped so it still prints if the port never comes up.
  ( n=0; until curl -sf -o /dev/null --max-time 2 http://localhost:7654/api/version || [ $n -ge 180 ]; do n=$((n + 1)); sleep 1; done; checkVersions ) &
  # On fd 3 (the real stdout), not the tee: ah4c writes /tmp/ah4c.log itself.
  exec ./ah4c >&3 2>&3
}

main
