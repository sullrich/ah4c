#!/bin/bash
FLAVOR=linux
STUB=$(dirname $(dirname $(dirname $(realpath $0))))
if [[ -z "${STUB}" || "${STUB}" = "/" ]]; then exit 12; fi
COMMON_DIR=${STUB}/chromecast/kodi_faves
source ${COMMON_DIR}/common.sh
DO_THIS=$(basename ${0%.*})
${DO_THIS} "$@" 1>&2
