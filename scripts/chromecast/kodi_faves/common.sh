#!/bin/bash

# Shell script debugging on if uncommented
set -x

#Trap end of script run
finish() {
    echo "$0 is exiting for ${STREAMER_IP} with exit code $?"
}
trap finish EXIT

## We use the kodi JSONRPC API over HTTP to control the kodi app. This will
## be the same host as the adb target, but the port number will be different.
## The kodi default is 8080, but you might have changed it.
##
CONFIG_KODI_JSONRPC_PORT="8080"

## kodi uses HTTP basic authentication for JSONRPC API over HTTP. The default
## userid is shown here, but you might have changed it when you turned on
## HTTP access.
CONFIG_KODI_JSONRPC_USERNAME="kodi"

## kodi forces password authentication for the JSONRPC API over HTTP. You set
## that password when you turned on HTTP access. The script cannot guess
## what it is. I hope you picked a good one.
##
CONFIG_KODI_JSONRPC_PASSWORD=""

## You can access the JSONRPC API over http or https. Change this config if
## you configured TLS/SSL when turning on HTTP access.
##
CONFIG_KODI_JSONRPC_SCHEME="http"

## There can be some kodi startup delay (splash screen, etc) before we can
## successfully switch to the favouritesbrowser window. Rather than some
## fixed delay, we keep trying until success or this many tries. If we try
## this many times, we give up and fail.
##
CONFIG_KODI_FAVOURITES_ITERATING_MAX="20"

## Sometimes a stream will fail to play for spurious reasons. The scripts can
## sometimes detect that and can retry some number of times. The configured
## value is the number of retries, not including the original play attempt.
##
CONFIG_KODI_RETRY_PLAYING_STREAM_MAX="2"

## After trying or retrying to play a stream, the script waits this long to
## check to see if it's actually playing. You want this delay to be long
## enough so that legitimate slow start-ups don't get misintepreted as
## failures.
##
CONFIG_SETTLE_ITERATING_KODI_FAILED_STREAM_RETRY="15"

## "Quit" here means the kodi graceful exit, as opposed to a force-stop.
## The idea is to minimize potential carnage that a force-stop might give.
##
CONFIG_STOP_DOES_KODI_QUIT="false"

## If we do a kodi quit, this gives a little time for it to do its thing
## before a possible force-stop. (I'm not sure if the quit process has
## completed by the time the RPC call returns.)
##
CONFIG_SETTLE_AFTER_KODI_QUIT="2"

## I'm not sure what happens if kodi goes into the background without a
## quit or force-stop. Is it still streaming something over the network?
## This tracks down all the active players and stops them. It can also
## eliminate a brief flash of the previously tuned channel when starting
## a new tuning.
##
CONFIG_KODI_STOP_DOES_PLAYERS_STOP="true"

## When done with a stream, should we do a force-stop on the streaming app?
##
CONFIG_STOP_DOES_APP_FORCE_STOP="false"

## When done with a stream, should we send a HOME button press to the device?
##
CONFIG_STOP_DOES_DEVICE_HOME="true"

## When done with a video stream, should the Android device be put to
## sleep? I don't know if this saves any significant resources, since
## there is no screen, but it might ease the burden on the HDMI
## encoder device; mine switches to a static image. Set this to false
## if it seems like Channels has problems when the device needs waking
## up.
##
CONFIG_STOP_DOES_DEVICE_SLEEP="false"

## If you opt to put the device to sleep, you may find it useful to
## wait for the screen to come back on in the prebmitune.sh
## step. Regardless, we always check for screen on in the bmitune.sh
## step.
##
CONFIG_PRETUNE_WAIT_FOR_SCREEN="true"

## The app navigation should go OK regardless of where it last was
## if it's still running. If for some reason that isn't working
## correctly, you can force-stop it first.
##
CONFIG_FORCE_STOP_BEFORE_APP_START="false"

## See the "DELAYS" section in README.txt for some considerations.
CONFIG_DELAY_SCALING="1"
CONFIG_DELAY_OFFSET="0"

CONFIG_SETTLE_ITERATING_FOR_SCREEN_ON="0.5"
CONFIG_SETTLE_ITERATING_FOR_BMITUNE_DONE="2"

CONFIG_SETTLE_AFTER_SCREEN_ON="0.25"
CONFIG_SETTLE_AFTER_FORCE_STOP="0.25"

## When waiting for the favouritesbrowser window, we delay this long
## between retries.
##
CONFIG_SETTLE_ITERATING_KODI_ACTIVATING_FAVOURITES="1"

###################################################################
# end of user configuration options ... don't change things below #
###################################################################
APP_PACKAGE="org.xbmc.kodi"
APP_ACTIVITY=".Main"

## "config-local.sh" is optional and allows for overriding config values without directly editing the scripts.
LF=`dirname $0`/config-local.sh
if [ -f "${LF}" ]
then
    . "${LF}"
fi

# We need jq for JSON fiddling, but the ah4c docker image doesn't have it by default.
# We only need to install it after a docker container re-create or whatever.
ensureWeHaveJQ() {
    type jq >/dev/null 2>&1 || apk add jq
}

# Functions with prefix "kodi" are specific to the kodi app. Other functions are more or less generic.

# define a few adb command fragments ("R_" is mnemonic for "remote control" even though some are not buttons on the remote)
R_SLEEP="shell input keyevent KEYCODE_SLEEP"
R_HOME="shell input keyevent KEYCODE_HOME"
R_FORCE_STOP="shell am force-stop"
R_LAUNCH_APP="shell am start -W -n"

# kodi JSONRPC info:
# https://kodi.wiki/view/JSON-RPC_API
# https://kodi.wiki/view/JSON-RPC_API/Examples

J_ACTIVATE_FAVOURITES='{"jsonrpc": "2.0", "method": "GUI.ActivateWindow", "params": {"window": "favouritesbrowser"}, "id": 1}'
J_INPUT_DOWN='{"jsonrpc": "2.0", "method": "Input.Down", "id": 1}'
J_INPUT_UP='{"jsonrpc": "2.0", "method": "Input.Up", "id": 1}'
J_INPUT_SELECT='{"jsonrpc": "2.0", "method": "Input.Select", "id": 1}'
J_INPUT_BACK='{"jsonrpc": "2.0", "method": "Input.Back", "id": 1}'
J_APPLICATION_QUIT='{"jsonrpc": "2.0", "method": "Application.Quit", "id": 1}'
J_PLAYER_STOP='{"jsonrpc": "2.0", "method": "Player.Stop", "params": {"playerid": PLAYERID}, "id": 1}'

J_GET_FAVES='{"jsonrpc": "2.0", "method": "Favourites.GetFavourites", "id": 1}'
# {
#   "id": 1,
#   "jsonrpc": "2.0",
#   "result": {
#     "favourites": [
#       {
#         "path": "plugin://slyguy.pbs.live/?_=play&_play=1&callsign=KCTS&_is_live=1&_noresume=.pvr",
#         "thumbnail": "http://127.0.0.1:52103/https://image.pbs.org/stations/kcts-color-cobranded-logo-lBlyOon.png|user-agent=okhttp/4.9.3&session_type=art&session_addonid=slyguy.pbs.live",
#         "title": "Cascade PBS",
#         "type": "media"
#       },
#       {
#         "path": "plugin://slyguy.pbs.live/?_=play&_play=1&callsign=KBTC&_is_live=1&_noresume=.pvr",
#         "thumbnail": "http://127.0.0.1:52103/https://image.pbs.org/stations/kbtc-color-cobranded-logo-UQTavrW.png|user-agent=okhttp/4.9.3&session_type=art&session_addonid=slyguy.pbs.live",
#         "title": "KBTC Public Television",
#         "type": "media"
#       }
#     ],
#     "limits": {
#       "end": 2,
#       "start": 0,
#       "total": 2
#     }
#   }
# }

J_GUI_GETPROPERTIES='{"jsonrpc": "2.0", "method": "GUI.GetProperties", "params": {"properties": ["currentwindow", "currentcontrol"]}, "id": 1}'
# {
#   "id": 1,
#   "jsonrpc": "2.0",
#   "result": {
#     "currentcontrol": {
#       "label": "Cascade PBS"
#     },
#     "currentwindow": {
#       "id": 10060,
#       "label": "Favourites"
#     }
#   }
# }

J_ACTIVE_PLAYERS='{"jsonrpc": "2.0", "method": "Player.GetActivePlayers", "id": 1}'
# {
#   "id": 1,
#   "jsonrpc": "2.0",
#   "result": [
#     {
#       "playerid": 1,
#       "playertype": "internal",
#       "type": "video"
#     },
#     {
#       "playerid": 3,
#       "playertype": "internal",
#       "type": "audio"
#     }
#   ]
# }
#

## The GUI ID for the Favourites window. We use the ID instead of the label
## to avoid localization issues. See kodi source file xbmc/guilib/WindowIDs.h
##
KODI_FAVOURITES_WINDOW_ID="10060"

## The GUI ID for the video player window.  We use the ID instead of the label
## to avoid localization issues. See kodi source file xbmc/guilib/WindowIDs.h
##
KODI_PLAYER_WINDOW_ID="12005"

init() {
    STREAMER_WITH_PORT="$1"
    STREAMER_NO_PORT="${STREAMER_WITH_PORT%%:*}"
    ADB_="adb -s ${STREAMER_WITH_PORT}"
    JSONRPC_="curl -s -u ${CONFIG_KODI_JSONRPC_USERNAME}:${CONFIG_KODI_JSONRPC_PASSWORD} --url ${CONFIG_KODI_JSONRPC_SCHEME}://${STREAMER_NO_PORT}:${CONFIG_KODI_JSONRPC_PORT}/jsonrpc --json"

    ensureWeHaveJQ
}

## At various points, we have to wait for the physical device to do something. We call that
## "settling time". Value in seconds, not limited to integers. See README.txt.
settle() {
    # we have to use bc because the terms are floating point and bash can only do integers
    # avoid bc's truncating division
    calculation="(${1} * ${CONFIG_DELAY_SCALING}) + (${CONFIG_DELAY_OFFSET})"
    delay=`echo "${calculation}" | bc -q`
    echo "settle ${calculation}"
    if [ "${delay:0:1}" != "-" ]
    then
	sleep "${delay}"
    fi
}

jsonrpc() {
    echo "$1" | jq . >&2
    local result=`$JSONRPC_ "$1"`
    local code="$?"
    echo "$result" | jq . >&2
    echo "$result"
    return "$code"
}

waitForWakeUp() {
    local -i maxRetries=15
    local -i retryCounter=0

    while true
    do
	# Fire TV returns dumpsys output with CRLF line endings; what an annoyance
	displayState=` $ADB_ shell "input keyevent KEYCODE_WAKEUP ; dumpsys display" | grep -e 'mGlobalDisplayState=' -e 'Display State=' -m 1 | cut -d= -f2 | tr -d '\r\n' `
	if [ ${displayState} = "ON" ]; then
	    break
	fi

	if ((${retryCounter} > ${maxRetries})); then
	    touch $STREAMER_NO_PORT/adbCommunicationFail
	    echo "Communication with ${STREAMER_WITH_PORT} failed after ${maxRetries} retries"
	    forceStopAndExit 1
	fi

	# we need to beat a relatively short Channels connection timeout (8-10 seconds), so we use only short sleeps per iteration
	settle "${CONFIG_SETTLE_ITERATING_FOR_SCREEN_ON}"
	((retryCounter++))
    done
    settle ${CONFIG_SETTLE_AFTER_SCREEN_ON}
}


# Check if bmitune.sh is done running. The stopbmitune.sh script might be called while bmitune.sh is still going.
waitForBmituneDone() {
    bmitunePID=$(<"$STREAMER_NO_PORT/bmitune_pid")

    while ps -p ${bmitunePID} > /dev/null 2>&1 ; do
	echo "Waiting for bmitune.sh to complete..."
	settle "${CONFIG_SETTLE_ITERATING_FOR_BMITUNE_DONE}"
    done
}

# either way, this should leave us at the app startup main page
launchTheApp() {
    if [ "${CONFIG_FORCE_STOP_BEFORE_APP_START}" = "true" ]
    then
	echo "FORCE STOP ${APP_PACKAGE}"
        $ADB_ ${R_FORCE_STOP} "${APP_PACKAGE}"
	settle "${CONFIG_SETTLE_AFTER_FORCE_STOP}"
    fi

    echo "STARTING ${APP_PACKAGE}/${APP_ACTIVITY}"
    $ADB_ ${R_LAUNCH_APP} "${APP_PACKAGE}/${APP_ACTIVITY}"
    touch $STREAMER_NO_PORT/adbAppRunning
}

forceStopAndExit() {
    echo "Doing force-stop of ${APP_PACKAGE}"
    $ADB_ ${R_FORCE_STOP} ${APP_PACKAGE}
    exit "$1"
}

kodiStopTheApp() {
    if [ "${CONFIG_KODI_STOP_DOES_PLAYERS_STOP}" = "true" ]
    then
	kodiStopThePlayers
    fi
    if [ "${CONFIG_STOP_DOES_KODI_QUIT}" = "true" ]
    then
	jsonrpc "${J_APPLICATION_QUIT}"
	settle ${CONFIG_SETTLE_AFTER_KODI_QUIT}
    fi
    if [ "${CONFIG_STOP_DOES_APP_FORCE_STOP}" = "true" ]
    then
	$ADB_ ${R_FORCE_STOP} ${APP_PACKAGE}
    fi
    if [ "${CONFIG_STOP_DOES_DEVICE_HOME}" = "true" ]
    then
	$ADB_ ${R_HOME}
    fi
    echo "Streaming stopped for ${STREAMER_WITH_PORT}"
}

putTheDeviceToSleep() {
    $ADB_ ${R_SLEEP}
    echo "Device sleep initiated for ${STREAMER_WITH_PORT}"
}

updateReferenceFiles() {
  # Handle cases where stream_stopped or last_channel don't exist
  mkdir -p $STREAMER_NO_PORT
  [[ -f "$STREAMER_NO_PORT/stream_stopped" ]] || echo 0 > "$STREAMER_NO_PORT/stream_stopped"
  [[ -f "$STREAMER_NO_PORT/last_channel" ]] || echo 0 > "$STREAMER_NO_PORT/last_channel"

  # Write PID for this script to bmitune_pid for use in stopbmitune.sh
  echo $$ > "$STREAMER_NO_PORT/bmitune_pid"
  echo "Current PID for this script is $$"
}

# Set encoderURL based on the value of streamer IP. This is for multi-encoder environments.
matchEncoderURL() {
  case "${STREAMER_WITH_PORT}" in
    "$TUNER1_IP") encoderURL=$ENCODER1_URL ;;
    "$TUNER2_IP") encoderURL=$ENCODER2_URL ;;
    "$TUNER3_IP") encoderURL=$ENCODER3_URL ;;
    "$TUNER4_IP") encoderURL=$ENCODER4_URL ;;
    *) forceStopAndExit 1 ;;
  esac
}

#Special channels to kill app or reboot device
specialChannels() {

    if [ "${REQUESTED_THING}" = "exit" ]; then
      echo "Exit $APP_PACKAGE requested on ${STREAMER_WITH_PORT}"
      rm $STREAMER_NO_PORT/last_channel $STREAMER_NO_PORT/adbAppRunning
      $ADB_ shell am force-stop $APP_PACKAGE
      exit 0
    elif [ "${REQUESTED_THING}" = "reboot" ]; then
      echo "Reboot ${STREAMER_WITH_PORT} requested"
      rm $STREAMER_NO_PORT/last_channel $STREAMER_NO_PORT/adbAppRunning
      $ADB_ reboot
      exit 0
    elif [[ -f $STREAMER_NO_PORT/adbCommunicationFail ]]; then
      rm $STREAMER_NO_PORT/adbCommunicationFail
      # earlier code had an error exit here since this file was a flag, but these scripts to do their own error exits
    else
      echo "Not a special channel (exit nor reboot)"
    fi
}

kodiGetCurrentControlLabel() {
    jsonrpc "${J_GUI_GETPROPERTIES}" | jq -r .result.currentcontrol.label
}

kodiGetCurrentWindowId() {
    jsonrpc "${J_GUI_GETPROPERTIES}" | jq -r .result.currentwindow.id
}

kodiActivateFavourites() {
    echo "Activating kodi favourites window"
    # We can't make this call until the kodi app is actually started. Iterate with retries. We can't
    # easily tell the difference between a simple start-up delay and something horribly wrong, so
    # there is a limit to how many retries we'll do.

    local -i tryCounter=0
    while true
    do
	jsonrpc "${J_ACTIVATE_FAVOURITES}"
	if [ "$?" == 0 ]
	then
	    local window=`kodiGetCurrentWindowId`
	    echo "Current window is '${window}'"
	    if [ "${window}" = "${KODI_FAVOURITES_WINDOW_ID}" ]
	       then
		   echo "Favourites window is activated"
		   break
	    fi
	fi
	
	if ((${tryCounter} > ${CONFIG_KODI_FAVOURITES_ITERATING_MAX})); then
	    touch $STREAMER_NO_PORT/adbCommunicationFail
	    echo "Activating favourites window on ${STREAMER_WITH_PORT} failed after ${CONFIG_KODI_FAVOURITES_ITERATING_MAX} tries"
	    forceStopAndExit 1
	fi
	((tryCounter++))
	settle ${CONFIG_SETTLE_ITERATING_KODI_ACTIVATING_FAVOURITES}
    done
}

kodiFindPositionsInFavourites() {
    local favouritesList="$1"
    local originalSelection="$2"
    local tuningPattern="$3"
    # complete line, exact match
    local -i originalPosition=`echo "${favouritesList}" | jq -r '.result.favourites[].title' | grep -F -x -n "${originalSelection}" | cut -f1 -d:`
    # any part of line, case independent match
    local -i tunePosition=`    echo "${favouritesList}" | jq -r '.result.favourites[].title' | grep -i -F -n "${tuningPattern}"     | cut -f1 -d:`
    echo "${tunePosition} ${originalPosition}"
}

kodiNavigateFavourites() {
    # It would be great to just play the desired item from the favourites list, but I didn't
    # figure out a way to do that. Instead, navigate to it with up or down motions and select it.
    local tuningPattern="${TUNING_HINT,,}"
    echo "tuningPattern '${tuningPattern}'"
    local originalSelection=`kodiGetCurrentControlLabel`
    echo "originalSelection '${originalSelection}'"
    
    local favouritesList=`jsonrpc "${J_GET_FAVES}"`
    local -i favdex="0"
    local -i originalPosition=0
    local -i tunePosition=0
    local result=`kodiFindPositionsInFavourites "$favouritesList" "$originalSelection" "$tuningPattern"`
    read tunePosition originalPosition <<<${result}
    if [ -z "${originalSelection}" -o "${originalPosition}" -eq 0 ]
    then
	# It can sometimes happen that nothing is the current item; simply move down to highlight something.
	# (I'm not sure how/why that happens.)
	# The list in kodi wraps around, so we don't have to worry about hitting either end.
	jsonrpc "${J_INPUT_DOWN}"
	originalSelectionl=`kodiGetCurrentControlLabel`
	result=`kodiFindPositionsInFavourites "$favouritesList" "$originalSelection" "$tuningPattern"`
	read tunePosition originalPosition <<<${result}
    fi
    if [ ${originalPosition} -eq 0 ]
    then
	echo "Something is horribly wrong is searching the favourites list. No current selection."
	forceStopAndExit 3
    fi
    if [ ${tunePosition} -eq 0 ]
    then
	echo "You tried to tune a thing, '${tuningPattern}', that is not in your favourites list."
	forceStopAndExit 4
    fi

    local movementCommand="$J_INPUT_DOWN"
    local -i movesRemaining=0
    if [ ${tunePosition} -gt ${originalPosition} ]
    then
	movementCommand="$J_INPUT_DOWN"
	movesRemaining=$(($tunePosition - $originalPosition))
    elif [ ${tunePosition} -lt ${originalPosition} ]
    then
	movementCommand="$J_INPUT_UP"
	movesRemaining=$(($originalPosition - $tunePosition))
    fi
    while [ "$movesRemaining" -gt 0 ]
    do
	jsonrpc "$movementCommand"
	((movesRemaining--))
    done
    jsonrpc "${J_INPUT_SELECT}"
}

kodiTune() {
    local -i tryCounter=0
    while true
    do
	kodiActivateFavourites
	kodiNavigateFavourites
	settle ${CONFIG_SETTLE_ITERATING_KODI_FAILED_STREAM_RETRY}
        local label=`kodiGetCurrentWindowId`
        if [ "${label}" = "${KODI_PLAYER_WINDOW_ID}" ]
        then
            echo "Stream playing after ${tryCounter} tries."
            break
        fi
	# do "back" to attempt to cleary any error pop-up
	jsonrpc "${J_INPUT_BACK}"
	if ((${tryCounter} > ${CONFIG_KODI_RETRY_PLAYING_STREAM_MAX})); then
	    touch $STREAMER_NO_PORT/adbCommunicationFail
	    echo "Stream failed to play after ${CONFIG_KODI_RETRY_PLAYING_STREAM_MAX} tries"
	    forceStopAndExit 1
	fi
	((tryCounter++))
    done
}

kodiStopThePlayers() {
    # I don't know if it's meaningful to do this in our context, but it finds all the active players and tells them to stop.
    jsonrpc "${J_ACTIVE_PLAYERS}" | jq -r '.result[].playerid' |
	while read playerid
	do
	    local j=`echo "${J_PLAYER_STOP}" | sed s/PLAYERID/${playerid}/`
	    echo "Stopping playerid ${playerid}"
	    jsonrpc "${j}"
	done
}

stopbmitune() {
    init "$1"
    waitForBmituneDone
    kodiStopTheApp
    if [ "${CONFIG_STOP_DOES_DEVICE_SLEEP}" = "true" ]
    then
	putTheDeviceToSleep
    fi
    date +%s > $STREAMER_NO_PORT/stream_stopped
    echo "$STREAMER_NO_PORT/stream_stopped written"
}

prebmitune() {
    init "$1"
    mkdir -p $STREAMER_NO_PORT
    adb connect ${STREAMER_WITH_PORT}
    # fire and forget the wakeup call to make sure the screen comes back on soon if it was off
    $ADB_ shell input keyevent KEYCODE_WAKEUP
    WAKEUP_EXIT_CODE="$?"
    if [ "${CONFIG_PRETUNE_WAIT_FOR_SCREEN}" = "true" ]
    then
	waitForWakeUp
    else
	forceStopAndExit ${WAKEUP_EXIT_CODE}
    fi
}

bmitune() {
    init "$2"
    IFS=_ read TAG FAVES TUNING_HINT <<<${1}
    REQUESTED_THING="$1"
    echo "REQUESTED_THING is '${REQUESTED_THING}'"
    echo "TUNING_HINT is '${TUNING_HINT}'"
    if [ ${FAVES} != "favourites" -a ${FAVES} != "favorites" ]
    then
        echo "We don't know what to do with ${FAVES}".
        forceStopAndExit 1
    fi
    
    launchTheApp
    updateReferenceFiles
    matchEncoderURL
    specialChannels
    waitForWakeUp
    kodiTune
}
