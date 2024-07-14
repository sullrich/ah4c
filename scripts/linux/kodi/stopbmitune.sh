#!/bin/bash
FLAVOR=linux
cd `dirname $0`/../../chromecast/kodi_faves && source common.sh
stopbmitune "$@" 1>&2
