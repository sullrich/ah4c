#!/bin/bash
FLAVOR=linux
cd `dirname $0`/../../chromecast/kodi_faves && source common.sh
bmitune "$@" 1>&2
