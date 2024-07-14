#!/bin/bash
FLAVOR=android
cd `dirname $0`/../../chromecast/kodi_faves && source common.sh
prebmitune "$@" 1>&2
