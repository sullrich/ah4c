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

## Tuning involves looking for a desired item among things on the favorites
## list. That goes pretty fast, and this is just a backstop to prevent a
## bug for an infinite loop. We shouldn't ever need this. If you really have
## more than this many items in your favorites list, you'll have to change
## this config value.
#
CONFIG_KODI_FAVOURITES_SIZE_MAX="100"

## There can be some kodi startup delay (splash screen, etc) before we can
## successfully switch to the favouritesbrowser window. Rather than some
## fixed delay, we keep trying until success or this many tries.
##
CONFIG_KODI_FAVOURITES_ITERATING_MAX="20"

## The GUI label for the Favourites window. Might be different if you don't use the
## default kodi localization. This config value should be lowercase.
##
CONFIG_KODI_FAVOURITES_WINDOW_LABEL="favourites"

## The GUI label for the video player window. Might be different if you don't use the
## default kodi localization. This config value should be lowercase.
##
CONFIG_KODI_PLAYER_WINDOW_LABEL="fullscreen video"

## Sometimes a stream will fail to play for spurious reasons. The scripts can
## sometimes detect that and can retry some number of times. The configured
## value is the number of tries, not including the original play attempt.
##
CONFIG_KODI_TRY_PLAYING_STREAM_MAX="2"

## After trying or retrying to play a stream, the script waits this long to
## check to see if it's actually playing. You want this delay to be long
## enough so that legitimate slow start-ups don't get misintepreted as
## failures.
##
CONFIG_SETTLE_ITERATING_KODI_FAILED_STREAM_RETRY="10"

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
# not used
#J_GET_FAVES='{"jsonrpc": "2.0", "method": "Favourites.GetFavourites", "id": 1}'
J_GUI_GETPROPERTIES='{"jsonrpc": "2.0", "method": "GUI.GetProperties", "params": {"properties": ["currentwindow", "currentcontrol"]}, "id": 1}'

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

stopTheApp() {
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

#Device sleep
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
      forceStopAndExit 1
    else
      echo "Not a special channel (exit nor reboot)"
    fi
}

kodiGetCurrentControlLabel() {
    echo `$JSONRPC_ "${J_GUI_GETPROPERTIES}" | jq -r .result.currentcontrol.label | tr '[:upper:]' '[:lower:]'`
}

kodiGetCurrentWindowLabel() {
    echo `$JSONRPC_ "${J_GUI_GETPROPERTIES}" | jq -r .result.currentwindow.label | tr '[:upper:]' '[:lower:]'`
}

kodiActivateFavourites() {
    echo "Activating kodi favourites window"
    # We can't make this call until the kodi app is actually started. Iterate with retries. We can't
    # easily tell the difference between a simple start-up delay and something horribly wrong, so
    # there is a limit to how many retries we'll do.

    local -i tryCounter=0
    while true
    do
	$JSONRPC_ "${J_ACTIVATE_FAVOURITES}"
	if [ "$?" == 0 ]
	then
	    local window=`kodiGetCurrentWindowLabel`
	    echo "Current window is '${window}'"
	    if [ "${window}" = "${CONFIG_KODI_FAVOURITES_WINDOW_LABEL}" ]
	       then
		   echo "Favorites window is activated"
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

kodiNavigateFavourites() {
    local seeking
    seeking=`echo "${TUNING_HINT}" | tr  '[:upper:]' '[:lower:]'`
    echo "seeking '${seeking}'"
    # it can sometimes happen that nothing is selected; simply move up or down to highlight something
    # the list in kodi wraps around, so we don't have to worry about hitting either end
    $JSONRPC_ "${J_INPUT_DOWN}"
    $JSONRPC_ "${J_INPUT_UP}"
    local label
    label=`kodiGetCurrentControlLabel`
    if [ -z "${label}}" ]
    then
        echo "Something is very wrong. Is your kodi favourites list empty?"
        forceStopAndExit 1
    fi
    local loopStopper
    loopStopper="${label}"
    
    # This is a simple loop that keeps going down and checking if we are in the right place.
    # If we get all the way back around to where we started, we know the desired item is
    # not in the favorites list. That's obviously inefficient if the favorites list is
    # large; in the worst case we might traverse the entire list. A more efficient way
    # would be to get the favorites list (see J_GET_FAVES), locate the current item and
    # desired items in that list, and then move either up or down in the fewest number of
    # moves to get from current to desired. But that's a lot of bother....
    local -i tryCounter=0
    while true
    do
        if [[ "${label}" =~ "${seeking}" ]]
        then
            echo "Found '${label}' after ${tryCounter} tries. Selecting."
            $JSONRPC_ "${J_INPUT_SELECT}"
            break
        fi
        $JSONRPC_ "${J_INPUT_DOWN}"
        label=`kodiGetCurrentControlLabel`
        if [ "${label}" = "${loopStopper}" ]
        then
            # round and round and round she went....
            echo "The desired item '${seeking}' was not found in the favourites list."
            forceStopAndExit 1
        fi

	if ((${tryCounter} > ${CONFIG_KODI_FAVOURITES_SIZE_MAX})); then
	    touch $STREAMER_NO_PORT/adbCommunicationFail
	    echo "Communication with ${STREAMER_WITH_PORT} failed after ${CONFIG_KODI_FAVOURITES_SIZE_MAX} tries"
	    forceStopAndExit 1
	fi
	((tryCounter++))
    done
}

kodiTune() {
    local -i tryCounter=0
    while true
    do
	kodiActivateFavourites
	kodiNavigateFavourites
	settle ${CONFIG_SETTLE_ITERATING_KODI_FAILED_STREAM_RETRY}
        local label=`kodiGetCurrentWindowLabel`
        if [ "${label}" = "${CONFIG_KODI_PLAYER_WINDOW_LABEL}" ]
        then
            echo "Stream playing after ${tryCounter} tries."
            break
        fi
	# do "back" to attempt to cleary any error pop-up
	$JSONRPC_ "${J_INPUT_BACK}"
	if ((${tryCounter} > ${CONFIG_KODI_TRY_PLAYING_STREAM_MAX})); then
	    touch $STREAMER_NO_PORT/adbCommunicationFail
	    echo "Stream failed to play after ${CONFIG_KODI_TRY_PLAYING_STREAM_MAX} tries"
	    forceStopAndExit 1
	fi
	((tryCounter++))
    done
}

stopbmitune() {
    init "$1"
    waitForBmituneDone
    stopTheApp
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
