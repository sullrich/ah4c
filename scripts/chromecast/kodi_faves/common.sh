#!/bin/bash

# Shell script debugging on if uncommented
set -x

#Trap end of script run
finish() {
    echo "$0 is exiting for ${STREAMER_NO_PORT} with exit code $?"
}
trap finish EXIT

## We use the kodi JSONRPC API over HTTP to control the kodi app. This will
## be the same host as the adb target, but the port number will be different.
## The kodi default is 8080, but you might have changed it.
##
CONFIG_KODI_JSONRPC_PORT="8080"

## You can access the JSONRPC API over http or https. Change this config if
## you configured TLS/SSL when turning on HTTP access.
##
CONFIG_KODI_JSONRPC_SCHEME="http"

## kodi uses HTTP basic authentication for JSONRPC API over HTTP. The default
## userid is shown here, but you might have changed it when you turned on
## HTTP access.
CONFIG_KODI_JSONRPC_USERNAME="kodi"

## kodi forces password authentication for the JSONRPC API over HTTP. You set
## that password when you turned on HTTP access. The script cannot guess
## what it is. I hope you picked a good one.
##
CONFIG_KODI_JSONRPC_PASSWORD=""

if [ ${FLAVOR} = "linux" ]
then
    ## For android kodi, we use adb, but for Linux kodi, we use ssh for a
    ## few things to run shell commands remotely. This is the SSH command
    ## for such cases. Everything except the target host name. This will
    ## typically include "-i" for an SSH key authentication, and "-l" for
    ## a username on the remote machine. If you are using a non-standard
    ## port, include the options for that here. If you want to use
    ## password authentication, you'll have to organize that via SSH
    ## ASKPASS, etc.
    ##
    ## ONLY NEEDED FOR LINUX FLAVOR
    CONFIG_KODI_SSH_COMMAND="ssh -i ./id_linuxkodi -l root -o StrictHostKeyChecking=accept-new"
fi

if [ ${FLAVOR} = "linux" ]
then
    ## There are lots of ways to start kodi in Linux environments. Instead
    ## of guessing, we ask you to tell us the path or command to something
    ## we should call to start kodi. This default value is the start-up
    ## script used by LibreELEC. This should be the entire shell command
    ## to start kodi.
    ##
    ## ONLY NEEDED FOR LINUX FLAVOR
    #CONFIG_LINUX_KODI_STARTER="/usr/lib/kodi/kodi.sh"
    ## Actually, under LibreELEC, kodi should always be running. Even if you kill it,
    ## LibreELEC just spawns it again. So, making this a no-op.
    CONFIG_LINUX_KODI_STARTER="echo 'assuming kodi is always running'"
fi

if [ ${FLAVOR} = "linux" ]
then
    ## To do the equivalent of a forced stop on Linux, we have to kill
    ## some process. Since there are so many different ways to run kodi,
    ## we need to be told what to kill. This should be the entire shell
    ## command to kill kodi.
    ##
    ## ONLY NEEDED FOR LINUX FLAVOR
    CONFIG_LINUX_KODI_STOPPER="killall -KILL kodi.sh"
fi

if [ ${FLAVOR} = "linux" ]
then
    ## For some Linux installs, like LibreELEC, you will be root and all
    ## need is a simple reboot command. For others, you may be some less
    ## privileged account and need to do it with "sudo" or some other
    ## special sauce.
    ##
    ## ONLY NEEDED FOR LINUX FLAVOR
    CONFIG_LINUX_KODI_REBOOTER="reboot"
fi

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

## "Quit" here means the kodi graceful exit, as opposed to a force-stop.
## The idea is to minimize potential carnage that a force-stop might give.
##
if [ ${FLAVOR} = "android" ]
then
    CONFIG_STOP_DOES_KODI_QUIT="true"
elif [ ${FLAVOR} = "linux" ]
then
    CONFIG_STOP_DOES_KODI_QUIT="false"
fi
  
## Ideally, any kodi players were stopped properly in the stopbmitune
## step. This is a sort of fallback in case our monkeying around gets
## kodi to lose its marbles. We stop all the players (if any) before
## starting a new one. Shouldn't ordinarily be necessary, but does no
## harm.
##
CONFIG_KODI_STOP_BEFORE_PLAY_START="true"

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

if [ ${FLAVOR} = "android" ]
then
    ## When done with a stream, should we send a HOME button press to the device?
    ##
    ## ONLY NEEDED FOR ANDROID FLAVOR
    CONFIG_STOP_DOES_DEVICE_HOME="true"
fi

if [ ${FLAVOR} = "android" ]
then
    ## When done with a video stream, should the Android device be put to
    ## sleep? I don't know if this saves any significant resources, since
    ## there is no screen, but it might ease the burden on the HDMI
    ## encoder device; mine switches to a static image. Set this to false
    ## if it seems like Channels has problems when the device needs waking
    ## up.
    ##
    ## ONLY NEEDED FOR ANDROID FLAVOR
    CONFIG_STOP_DOES_DEVICE_SLEEP="false"
fi

## If you opt to put the device to sleep, you may find it useful to
## wait for the screen to come back on in the prebmitune.sh
## step. Regardless, we always check for screen on in the bmitune.sh
## step. For the Linux flavor, "waking up" can mean deactivating the
## screensaver. It doesn't take long to deactivate the screensaver,
## so we don't bother with it in pretune for the Linux flavor.
##
if [ ${FLAVOR} = "android" ]
then
    CONFIG_PRETUNE_WAIT_FOR_SCREEN="true"
elif [ ${FLAVOR} = "linux" ]
then
    CONFIG_PRETUNE_WAIT_FOR_SCREEN="false"
fi

## Pre-tuning has competing and possibly conflicting desires. On the one hand
## it wants to take the least time possible so that Channels DVR does not time
## out and retry. On the other hand, problems that occur during tuning do not
## get communicated all the way back to Channels. If your tuning step is fast
## enough, you can work a compromise that does all of the tuning stuff as
## part of the pretuning script, leaving bmitune as a no-op. To do that, set
## this config item to "true".
##
CONFIG_PRETUNE_DOES_TUNE="true"

## There are two ways of tuning to a particular channel. You can either directly
## play it via a Player.Open call, or you can navigate through the Favourites panel.
## Player.Open is simpler and you should use that unless you encounter problems.
## Favourites navigation is here as a fallback, and also because it was implemented
## first.
##
CONFIG_PLAY_VIA_FAVOURITES_NAVIGATION="false"

## If there is a screensaver, it probably consumes the first keypress
## to deactivate itself. This probably only applies for Linux flavor
## but no harm done in using it with Android, too.
##
CONFIG_SCREENSAVER_EATS_KEY="true"

## The app navigation should go OK regardless of where it last was
## if it's still running. If for some reason that isn't working
## correctly, you can force-stop it first.
##
CONFIG_FORCE_STOP_BEFORE_APP_START="false"

## After tuning a channel, the script waits for the fullscreen player
## to actually appear. This is the maximum amount of seconds that it
## will wait before deciding it's not going to happen. Unlike the
## various "settle" configs, this is not a simple sleep, and the
## scaling stuff does not apply. The script checks for the fullscreen
## player roughly once a second. It stops looking when it finds it or
## this many seconds have elapsed. The value here must be an integer.
##
CONFIG_KODI_MAX_WAIT_FOR_PLAYER="15"

## See the "DELAYS" section in README.txt for some considerations.
CONFIG_DELAY_SCALING="1"
CONFIG_DELAY_OFFSET="0"

## If we do a kodi quit, this gives a little time for it to do its thing
## before a possible force-stop. (I'm not sure if the quit process has
## completed by the time the RPC call returns.)
##
CONFIG_SETTLE_AFTER_KODI_QUIT="2"

CONFIG_SETTLE_ITERATING_FOR_SCREEN_ON="0.5"
CONFIG_SETTLE_ITERATING_FOR_BMITUNE_DONE="2"

CONFIG_SETTLE_AFTER_SCREEN_ON="0.25"
CONFIG_SETTLE_AFTER_FORCE_STOP="0.25"

## When waiting for the favouritesbrowser window, we delay this long
## between retries.
##
CONFIG_SETTLE_ITERATING_KODI_ACTIVATING_FAVOURITES="1"

## When using JSONRPCs to move up or down the favourites list, settle
## this amount of teme after each step. I don't know if this is needed,
## but the small delay doesn't hurt unless the favourites list is huge.
##
CONFIG_SETTLE_ITERATING_FAVORITES="0.1"

## After all of the movement in the favourites list is done, settle
## this amount of time before finally selecting the highlighted item.
##
CONFIG_SETTLE_AFTER_NAVIGATING_FAVORITES="0.5"

## After the tuning action is completed and the channel starts playing,
## one final chance to settle a bit to let codec changes or whatever
## settle down. We've already waited for the player screen to show up,
## so you might not need anything here at all.
##
CONFIG_SETTLE_AFTER_TUNING="0.5"

###################################################################
# end of user configuration options ... don't change things below #
###################################################################
if [ ${FLAVOR} = "android" ]
then
    APP_PACKAGE="org.xbmc.kodi"
    APP_ACTIVITY=".Main"
fi

## "config-local.sh" is optional and allows for overriding config values without directly editing the scripts.
CONFIG_LOCAL=./config-local.sh
if [ -f "${CONFIG_LOCAL}" ]
then
    source "${CONFIG_LOCAL}"
fi

# We need jq for JSON fiddling, but the ah4c docker image doesn't have it by default.
# We only need to install it after a docker container re-create or whatever.
ensureWeHaveJQ() {
    type jq >/dev/null 2>&1 || apk add jq
}

ensureWeHaveSSH() {
    type ssh >/dev/null 2>&1 || apk add openssh-client
}

# Functions with prefix "kodi" are specific to the kodi app. Other functions are more or less generic.

if [ ${FLAVOR} = "android" ]
then
    # define a few adb command fragments ("R_" is mnemonic for "remote control" even though some are not buttons on the remote)
    R_SLEEP="shell input keyevent KEYCODE_SLEEP"
    R_HOME="shell input keyevent KEYCODE_HOME"
    R_FORCE_STOP="shell am force-stop"
    R_LAUNCH_APP="shell am start -W -n"
fi

# kodi JSONRPC info:
# https://kodi.wiki/view/JSON-RPC_API
# https://kodi.wiki/view/JSON-RPC_API/Examples

J_ACTIVATE_FAVOURITES='{"jsonrpc": "2.0", "method": "GUI.ActivateWindow", "params": {"window": "favouritesbrowser"}, "id": 1}'
J_INPUT_DOWN='{"jsonrpc": "2.0", "method": "Input.Down", "id": 1}'
J_INPUT_UP='{"jsonrpc": "2.0", "method": "Input.Up", "id": 1}'
J_INPUT_RIGHT='{"jsonrpc": "2.0", "method": "Input.Right", "id": 1}'
J_INPUT_LEFT='{"jsonrpc": "2.0", "method": "Input.Left", "id": 1}'
J_INPUT_SELECT='{"jsonrpc": "2.0", "method": "Input.Select", "id": 1}'
J_INPUT_BACK='{"jsonrpc": "2.0", "method": "Input.Back", "id": 1}'
J_APPLICATION_QUIT='{"jsonrpc": "2.0", "method": "Application.Quit", "id": 1}'
J_PLAYER_OPEN_HEAD='{"jsonrpc": "2.0", "method": "Player.Open", "params": {"item": {"file": "'
J_PLAYER_OPEN_TAIL='"}}, "id": 1}'
J_PLAYER_STOP_HEAD='{"jsonrpc": "2.0", "method": "Player.Stop", "params": {"playerid": '
J_PLAYER_STOP_TAIL='}, "id": 1}'

J_GET_FAVES='{"jsonrpc": "2.0", "method": "Favourites.GetFavourites", "params": {"properties": ["path"]}, "id": 1}'
# {
#   "id": 1,
#   "jsonrpc": "2.0",
#   "result": {
#     "favourites": [
#       {
#         "path": "plugin://slyguy.pbs.live/?_=play&_play=1&callsign=KCTS&_is_live=1&_noresume=.pvr",
#         "title": "Cascade PBS",
#         "type": "media"
#       },
#       {
#         "path": "plugin://slyguy.pbs.live/?_=play&_play=1&callsign=KBTC&_is_live=1&_noresume=.pvr",
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
    if [ ${FLAVOR} = "android" -o ${FLAVOR} = "linux" ]
    then
        echo "${FLAVOR} flavor of kodi"
    else
        echo "Unrecognized '${FLAVOR}' flavor of kodi"
        forceStopAndExit 1
    fi
    
    STREAMER_WITH_PORT="$1"
    STREAMER_NO_PORT="${STREAMER_WITH_PORT%%:*}"
    if [ ${FLAVOR} = "android" ]
    then
        REMOTE_COMMAND_="adb -s ${STREAMER_WITH_PORT}"
    elif [ ${FLAVOR} = linux ]
    then
        REMOTE_COMMAND_="${CONFIG_KODI_SSH_COMMAND} ${STREAMER_NO_PORT}"
    fi
    JSONRPC_="curl -s -u ${CONFIG_KODI_JSONRPC_USERNAME}:${CONFIG_KODI_JSONRPC_PASSWORD} --url ${CONFIG_KODI_JSONRPC_SCHEME}://${STREAMER_NO_PORT}:${CONFIG_KODI_JSONRPC_PORT}/jsonrpc --json"

    ensureWeHaveJQ
    if [ ${FLAVOR} = "linux" ]
    then
        ensureWeHaveSSH
    fi
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

    if [ ${FLAVOR} = "android" ]
    then
        while true
        do
            # Fire TV returns dumpsys output with CRLF line endings; what an annoyance
            displayState=` ${REMOTE_COMMAND_} shell "input keyevent KEYCODE_WAKEUP ; dumpsys display" | grep -e 'mGlobalDisplayState=' -e 'Display State=' -m 1 | cut -d= -f2 | tr -d '\r\n' `
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
    fi
    
    if [ ${CONFIG_PLAY_VIA_FAVOURITES_NAVIGATION} = "true" ]
    then
	settle ${CONFIG_SETTLE_AFTER_SCREEN_ON}
	if [ ${CONFIG_SCREENSAVER_EATS_KEY} = "true" ]
	then
            # We don't know if the screensaver is actually active.  By
            # using "right", which has no effect on the WideList view of
            # the favourites screen, it avoids a little screen flashing.
            # If we're on some other screen, we don't care what "right"
            # does (I hope :-) )
            jsonrpc "${J_INPUT_RIGHT}"
	fi
    fi
}


# Check if bmitune.sh is done running. The stopbmitune.sh script might be called while bmitune.sh is still going.
waitForBmituneDone() {
    bmitunePID=$(<"$STREAMER_NO_PORT/bmitune_pid")

    while ps -p ${bmitunePID} > /dev/null 2>&1 ; do
        echo "Waiting for bmitune.sh to complete..."
        settle "${CONFIG_SETTLE_ITERATING_FOR_BMITUNE_DONE}"
    done
}

forceStop() {
    echo "Force stop kodi"
    if [ ${FLAVOR} = "android" ]
    then
        ${REMOTE_COMMAND_} ${R_FORCE_STOP} "${APP_PACKAGE}"
    elif [ ${FLAVOR} = "linux" ]
    then
        ${REMOTE_COMMAND_} ${CONFIG_LINUX_KODI_STOPPER}
    fi
    settle "${CONFIG_SETTLE_AFTER_FORCE_STOP}"
}

# either way, this should leave us at the app startup main page
launchTheApp() {
    if [ "${CONFIG_FORCE_STOP_BEFORE_APP_START}" = "true" ]
    then
        forceStop
    fi

    if [ ${FLAVOR} = "android" ]
    then
	echo "STARTING ${APP_PACKAGE}/${APP_ACTIVITY}"
        ${REMOTE_COMMAND_} ${R_LAUNCH_APP} "${APP_PACKAGE}/${APP_ACTIVITY}"
    elif [ ${FLAVOR} = "linux" ]
    then
        ${REMOTE_COMMAND_} ${CONFIG_LINUX_KODI_STARTER}
    fi
    # not really adb for Linux, but going with the previous file naming
    touch $STREAMER_NO_PORT/adbAppRunning
}

forceStopAndExit() {
    forceStop
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
        forceStop
    fi
    if [ "${CONFIG_STOP_DOES_DEVICE_HOME}" = "true" ]
    then
        if [ ${FLAVOR} = "android" ]
        then
            ${REMOTE_COMMAND_} ${R_HOME}
        elif [ ${FLAVOR} = "linux" ]
        then
            : no Linux equivalent
        fi
    fi
    echo "Streaming stopped for ${STREAMER_WITH_PORT}"
}

putTheDeviceToSleep() {
    if [ ${FLAVOR} = "android" ]
    then
         ${REMOTE_COMMAND_} ${R_SLEEP}
    elif [ ${FLAVOR} = "linux" ]
    then
         : TODO Linux sleep not implemented
    fi
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
        forceStopAndExit 0
    elif [ "${REQUESTED_THING}" = "reboot" ]; then
        echo "Reboot ${STREAMER_WITH_PORT} requested"
        rm $STREAMER_NO_PORT/last_channel $STREAMER_NO_PORT/adbAppRunning
        if [ ${FLAVOR} = "android" ]
        then
             ${REMOTE_COMMAND_} reboot
        elif [ ${FLAVOR} = "linux" ]
        then
             ${REMOTE_COMMAND_} ${CONFIG_LINUX_KODI_REBOOTER}
        fi
        exit 0
    elif [[ -f $STREAMER_NO_PORT/adbCommunicationFail ]]; then
        rm $STREAMER_NO_PORT/adbCommunicationFail
        # earlier code had an error exit here since this file was a flag, but these scripts do their own error exits
    else
        echo "Not a special channel (not exit nor reboot)"
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
        if ((${tryCounter} > ${CONFIG_KODI_FAVOURITES_ITERATING_MAX})); then
            touch $STREAMER_NO_PORT/adbCommunicationFail
            echo "Activating favourites window on ${STREAMER_WITH_PORT} failed after ${CONFIG_KODI_FAVOURITES_ITERATING_MAX} tries"
            forceStopAndExit 1
        fi
        local window=`kodiGetCurrentWindowId`
        echo "Current window is '${window}'"
        if [ "${window}" = "${KODI_FAVOURITES_WINDOW_ID}" ]
        then
            echo "Favourites window is activated"
            break
        fi
        jsonrpc "${J_ACTIVATE_FAVOURITES}"
        
        ((tryCounter++))
        settle ${CONFIG_SETTLE_ITERATING_KODI_ACTIVATING_FAVOURITES}
    done
}

kodiFindPositionsInFavourites() {
    local favouritesList="$1"
    local originalSelection="$2"
    local tuningPattern="$3"
    # complete line, exact match
    local -i originalPosition
    if [ -z ${originalSelection} ]
    then
	# fake it
	originalPosition=0
    else
	originalPosition=` echo "${favouritesList}" | jq -r '.result.favourites[].title' | grep -F -x -n "${originalSelection}" | cut -f1 -d:`
    fi
    # any part of line, case independent match
    local -i tunePosition=`echo "${favouritesList}" | jq -r '.result.favourites[].title' | grep -i -F -n "${tuningPattern}"     | cut -f1 -d:`
    jqTunePosition=$((tunePosition-1))  # grep lines are 1-based, jq arrays are 0-based
    local tunePath=`echo "${favouritesList}" | jq -r ".result.favourites[$jqTunePosition].path"`
    echo "${tunePosition} ${originalPosition} ${tunePath}"
}

kodiNavigateFavourites() {
    # Navigate to the desired selection with up or down motions and select it.
    local tuningPattern="${TUNING_HINT,,}"
    echo "tuningPattern '${tuningPattern}'"
    local originalSelection=`kodiGetCurrentControlLabel`
    echo "originalSelection '${originalSelection}'"
    
    local favouritesList=`jsonrpc "${J_GET_FAVES}"`
    local -i favdex="0"
    local -i originalPosition=0
    local -i tunePosition=0
    local result=`kodiFindPositionsInFavourites "$favouritesList" "$originalSelection" "$tuningPattern"`
    read tunePosition originalPosition tunePath <<<${result}
    if [ -z "${originalSelection}" -o -z "${originalPosition}" -o "${originalPosition}" -eq 0 ]
    then
        # It can sometimes happen that nothing is the current item; simply move down to highlight something.
        # (I'm not sure how/why that happens.)
        # The list in kodi wraps around, so we don't have to worry about hitting either end.
        jsonrpc "${J_INPUT_DOWN}"
        originalSelection=`kodiGetCurrentControlLabel`
        result=`kodiFindPositionsInFavourites "$favouritesList" "$originalSelection" "$tuningPattern"`
        read tunePosition originalPosition tunePath <<<${result}
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
        settle ${CONFIG_SETTLE_ITERATING_FAVORITES}
        ((movesRemaining--))
    done
    settle ${CONFIG_SETTLE_AFTER_NAVIGATING_FAVORITES}
    jsonrpc "${J_INPUT_SELECT}"
}

kodiTuneViaPlayerOpen() {
    local tuningPattern="${TUNING_HINT,,}"
    echo "tuningPattern '${tuningPattern}'"
    
    local favouritesList=`jsonrpc "${J_GET_FAVES}"`
    local -i originalPosition=0
    local -i tunePosition=0
    local result=`kodiFindPositionsInFavourites "$favouritesList" "" "$tuningPattern"`
    read tunePosition originalPosition tunePath <<<${result}
    local j="${J_PLAYER_OPEN_HEAD}${tunePath}${J_PLAYER_OPEN_TAIL}"
    echo "Playing media ${tunePath}"
    jsonrpc "${j}"
}

kodiTuneViaFavouritesNavigation() {
    kodiActivateFavourites
    kodiNavigateFavourites
}

kodiTune() {
    if [ ${CONFIG_KODI_STOP_BEFORE_PLAY_START} = "true" ]
    then
        kodiStopThePlayers
    fi
    
    local -i tryCounter=0
    while true
    do
	if [ ${CONFIG_PLAY_VIA_FAVOURITES_NAVIGATION} = "true" ]
	then
	    kodiTuneViaFavouritesNavigation
	else
	    kodiTuneViaPlayerOpen
	fi

        local label
	local -i waitCounter=0
	while true
	do
	    label=`kodiGetCurrentWindowId`
	    ((waitCounter++))
            if [ "${label}" = "${KODI_PLAYER_WINDOW_ID}" -o ${waitCounter} -gt ${CONFIG_KODI_MAX_WAIT_FOR_PLAYER} ]
            then
		break
	    fi
	    sleep 1
	done
	
        if [ "${label}" = "${KODI_PLAYER_WINDOW_ID}" ]
        then
            local -i attempt=$((tryCounter+1))
            echo "Stream playing after ${attempt} tries."
            break
        fi
        # do "back" to attempt to clear any error pop-up
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
            local j="${J_PLAYER_STOP_HEAD}${playerid}${J_PLAYER_STOP_TAIL}"
            echo "Stopping playerid ${playerid}"
            jsonrpc "${j}"
        done
}

bmituneActual() {
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
    settle ${CONFIG_SETTLE_AFTER_TUNING}
}

# stopbmitune.sh 192.168.1.3:22 Tacoma_favorites_KBTC Public Television
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

# prebmitune.sh 192.168.1.3:22 Tacoma_favorites_KBTC Public Television
prebmitune() {
    init "$1"
    mkdir -p $STREAMER_NO_PORT
    # fire and forget the wakeup call to make sure the screen comes back on soon if it was off
    if [ ${FLAVOR} = "android" ]
    then
	adb connect ${STREAMER_WITH_PORT}
        ${REMOTE_COMMAND_} shell input keyevent KEYCODE_WAKEUP
        WAKEUP_EXIT_CODE="$?"
    elif [ ${FLAVOR} = "linux" ]
    then
        : Linux wakeup not implemented
        WAKEUP_EXIT_CODE="0"
    fi
    if [ "${CONFIG_PRETUNE_WAIT_FOR_SCREEN}" = "true" ]
    then
        waitForWakeUp
    fi

    if [ ${CONFIG_PRETUNE_DOES_TUNE} = "true" ]
    then
	bmituneActual "$2" "$1"
    fi
}

# bmitune.sh Tacoma_favorites_KBTC Public Television 192.168.1.3:22
bmitune() {
    if [ ${CONFIG_PRETUNE_DOES_TUNE} != "true" ]
    then
	bmituneActual "$@"
    fi
}
