#!/bin/bash
HOME="input keyevent KEYCODE_HOME; sleep 1"
PRIME1="input keyevent 19 19 19 19; sleep 1; input keyevent 21 21 21; sleep 1; input keyevent 22; sleep 1; input keyevent 23; sleep 1"
PRIME2="input keyevent 19; sleep 1"
PRIME3="input keyevent --longpress 67 67 67 67 67 67 67 67 67 67 67 67 67 67 67 67 67 67 67 67 67 67 67 67 67 67 67 67 67 67 67 67 67 67 67 67"
SEARCH1="input keyevent 66; sleep 2"
SEARCH2="input keyevent 66"

#USA
if [ $1 = "111" ];then
adb -s $2 shell $HOME
adb -s $2 shell $PRIME1
adb -s $2 shell $PRIME2
#adb -s $2 shell $PRIME3 
adb -s $2 shell input text "stream\ usa\ channel\ on\ YouTube\ TV"
adb -s $2 shell $SEARCH1
adb -s $2 shell $SEARCH2
fi

#SYFY
if [ $1 = "135" ];then
adb -s $2 shell $HOME
adb -s $2 shell $PRIME1
adb -s $2 shell $PRIME2
#adb -s $2 shell $PRIME3 
adb -s $2 shell input text "stream\ syfy\ on\ YouTube\ TV"
adb -s $2 shell $SEARCH1
adb -s $2 shell $SEARCH2
fi
