# Shell script debugging on if uncommented
set -x

#Trap end of script run
finish() {
    echo "$0 is exiting for ${STREAMER_IP} with exit code $?"
}
trap finish EXIT

## When done with a stream, should we do a force-stop on the streaming app?
##
CONFIG_STOP_DOES_APP_FORCE_STOP="true"

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

## The PBS app should be taken to the main panel, even if it's already
## running. If for some reason that isn't working correctly, you can
## force-stop it first. The idea is for the scripts to be in a known
## place when navigating.
##
CONFIG_FORCE_STOP_BEFORE_APP_START="true"

## Normall only go through station search when actually needed. This is a
## sort of failsafe that forces a station search after a period of time.
##
CONFIG_FORCE_STATION_CHANGE_AFTER="14400"

## See the "DELAYS" section in README.txt for some considerations.
CONFIG_DELAY_SCALING="1"
CONFIG_DELAY_OFFSET="0"

CONFIG_SETTLE_ITERATING_FOR_SCREEN_ON="0.5"
CONFIG_SETTLE_ITERATING_FOR_BMITUNE_DONE="2"
CONFIG_SETTLE_ITERATING_SEARCH_RESULTS="0.5"
CONFIG_SETTLE_ITERATING_APP_LAUNCH_CHECK="1"

CONFIG_SETTLE_AFTER_SCREEN_ON="0.6"
CONFIG_SETTLE_AFTER_LAUNCH_APP="3"
CONFIG_SETTLE_AFTER_KEY_INPUT="2"
CONFIG_SETTLE_MAIN_PANEL_TO_LIVE_PANEL="3"
CONFIG_SETTLE_BEFORE_WATCH_NOW="0.5"
CONFIG_SETTLE_AWAIT_SEARCH_RESULTS="4"
CONFIG_SETTLE_AFTER_SELECT_STATION="5"
CONFIG_SETTLE_AFTER_FORCE_STOP="0.4"

###################################################################
# end of user configuration options ... don't change things below #
###################################################################
APP_PACKAGE="com.pbs.video"
# using this bypasses the splash screen
APP_ACTIVITY=".tv.ui.home.TvMainActivity"

## "config-local.sh" is optional and allows for overriding config values without directly editing the scripts.
LF=`dirname $0`/config-local.sh
if [ -f "${LF}" ]
then
    . "${LF}"
fi

# Functions with prefix "pbs" are specific to the PBS app. Other functions are more or less generic.

# define a few adb command fragments ("R_" is mnemonic for "remote control" even though some are not buttons on the remote)
R_ENTER="shell input keyevent KEYCODE_ENTER"
R_RIGHT="shell input keyevent KEYCODE_DPAD_RIGHT"
R_CENTER="shell input keyevent KEYCODE_DPAD_CENTER"
R_DOWN="shell input keyevent KEYCODE_DPAD_DOWN"
R_SLEEP="shell input keyevent KEYCODE_SLEEP"
R_HOME="shell input keyevent KEYCODE_HOME"
R_TEXT="shell input text"
R_FORCE_STOP="shell am force-stop"
R_LAUNCH_APP="shell am start -W -n"

init() {
    STREAMER_WITH_PORT="$1"
    STREAMER_NO_PORT="${STREAMER_WITH_PORT%%:*}"
    ADB_="adb -s ${STREAMER_WITH_PORT}"
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
	    exit 1
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

waitForAppLaunch() {
    local -i maxRetries=15
    local -i retryCounter=0

    while true
    do
	focus=`getCurrentFocus`
	if ((${retryCounter} > ${maxRetries})); then
	    echo "App ${APP_PACKAGE} doesn't seem to be launching. Current focus is ${focus}"
	    exit 3
	fi
	if [ "${focus}" = "${APP_PACKAGE}/${APP_PACKAGE}${APP_ACTIVITY}" ]
	then
	    return 0
	fi
	settle "${CONFIG_SETTLE_ITERATING_APP_LAUNCH_CHECK}"
	((retryCounter++))
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
    # even though we did "-W" to wait, we might still need to wait a bit in case there is a splash screen or other lag
    waitForAppLaunch
    settle "${CONFIG_SETTLE_AFTER_LAUNCH_APP}"
    touch $STREAMER_NO_PORT/adbAppRunning
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
    *) exit 1 ;;
  esac
}

getCurrentFocus() {
    # Fire TV returns dumpsys output with CRLF line endings.
    appFocus=$($ADB_ shell dumpsys window | grep -E 'mCurrentFocus' | sed -e 's/.* //g' -e 's/}//' -e 's/\r//' )
    echo "${appFocus}"
}

#Special channels to kill app or reboot device
specialChannels() {

    if [ $REQUESTED_THING = "exit" ]; then
      echo "Exit $APP_PACKAGE requested on ${STREAMER_WITH_PORT}"
      rm $STREAMER_NO_PORT/last_channel $STREAMER_NO_PORT/adbAppRunning
      $ADB_ shell am force-stop $APP_PACKAGE
      exit 0
    elif [ $REQUESTED_THING = "reboot" ]; then
      echo "Reboot ${STREAMER_WITH_PORT} requested"
      rm $STREAMER_NO_PORT/last_channel $STREAMER_NO_PORT/adbAppRunning
      $ADB_ reboot
      exit 0
    elif [[ -f $STREAMER_NO_PORT/adbCommunicationFail ]]; then
      rm $STREAMER_NO_PORT/adbCommunicationFail
      exit 1
    else
      echo "Not a special channel (exit nor reboot)"
      appFocus=`getCurrentFocus`
      echo "Current app in focus is $appFocus" 
    fi
}

pbsFromLivePanelToChangeStationSearchPanel() {
    echo "navigate LIVE PANEL to SEARCH PANEL"
    echo "move RIGHT to 'change station' button"
    $ADB_ $R_RIGHT
    settle "${CONFIG_SETTLE_AFTER_KEY_INPUT}"
    echo "press 'change station' button"
    $ADB_ $R_CENTER
    settle "${CONFIG_SETTLE_AFTER_KEY_INPUT}"
}

pbsSearchForZipcodeViaSearchBox() {
    echo "search for term ${ZIPCODE}"
    echo "Move RIGHT to text search box"
    $ADB_ $R_RIGHT
    settle "${CONFIG_SETTLE_AFTER_KEY_INPUT}"
    echo "Send ZIPCODE ${ZIPCODE} to text search box"
    $ADB_ ${R_TEXT} "${ZIPCODE}"
    # need "enter" to execute the search (so we don't have to navigate)
    $ADB_ $R_ENTER
    settle "${CONFIG_SETTLE_AWAIT_SEARCH_RESULTS}"  # allow time for search results to come back
}

pbsNavigateSearchResults() {
    echo "navigate SEARCH RESULTS to POSITION ${POSITION}"
    for ((resultDex=1;resultDex<=POSITION;resultDex++)); do
	echo "Key DOWN to position ${resultDex}"
	$ADB_ $R_DOWN
	settle "${CONFIG_SETTLE_ITERATING_SEARCH_RESULTS}"
    done
    echo "SELECT station"
    $ADB_ $R_CENTER
    settle "${CONFIG_SETTLE_AFTER_SELECT_STATION}"
}

pbsMaybeChangeStation() {
    local lastChannel
    local lastAwake
    local timeNow
    local timeElapsed
    local maxTime="${CONFIG_FORCE_STATION_CHANGE_AFTER}"

    lastChannel=$(<"$STREAMER_NO_PORT/last_channel")
    lastAwake=$(<"$STREAMER_NO_PORT/stream_stopped")
    timeNow=$(date +%s)
    timeElapsed=$(($timeNow - $lastAwake))

    if [ ${lastChannel} = ${REQUESTED_THING} ] && (( $timeElapsed < $maxTime )); then
	echo "Last channel selected on this tuner (${lastChannel}), no channel change required"
    else
	if [ -f $STREAMER_NO_PORT/adbAppRunning ] && (( $timeElapsed < $maxTime )); then
	    rm $STREAMER_NO_PORT/adbAppRunning
	fi
	echo $REQUESTED_THING > "$STREAMER_NO_PORT/last_channel"
	pbsFromLivePanelToChangeStationSearchPanel
	pbsSearchForZipcodeViaSearchBox
	pbsNavigateSearchResults
	# brought us back to the app home screen
	pbsFromMainPanelToLivePanel
    fi
}

pbsFromMainPanelToLivePanel() {
    # select live TV
    echo "navigate MAIN PANEL to LIVE PANEL"
    $ADB_ $R_CENTER
    settle "${CONFIG_SETTLE_MAIN_PANEL_TO_LIVE_PANEL}"
}

pbsFromLivePanelToPlaying() {
    echo "navigate LIVE PANEL to PLAYING"
    $ADB_ $R_CENTER
    # no settle time needed
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
	exit ${WAKEUP_EXIT_CODE}
    fi
}

bmitune() {
    init "$2"
    IFS=_ read TAG ZIPCODE POSITION <<<${1}
    if [ -z ${POSITION} ]; then POSITION="1"; fi
    REQUESTED_THING="$1"
    echo REQUESTED_THING is ${REQUESTED_THING}

    launchTheApp
    updateReferenceFiles
    matchEncoderURL
    specialChannels
    waitForWakeUp
    pbsFromMainPanelToLivePanel
    # Tuning either does nothing but click the button, or it goes through the station selection dialog first
    # launchTheApp leaves us at the "live TV" panel
    if [ ! -z "${ZIPCODE}" ]
    then
        pbsMaybeChangeStation
    fi
    settle "${CONFIG_SETTLE_BEFORE_WATCH_NOW}"
    pbsFromLivePanelToPlaying
}
