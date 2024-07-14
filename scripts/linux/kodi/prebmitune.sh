#!/bin/bash
FLAVOR=linux
cd `dirname $0`/../../chromecast/kodi_faves && source common.sh
prebmitune "$@" 1>&2
