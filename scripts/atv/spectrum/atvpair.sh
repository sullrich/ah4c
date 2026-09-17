#!/bin/bash
# atvpair.sh for atv/spectrum
# 2026.08.05

set -euo pipefail

streamerIP="${1:-}"
if [[ -z "$streamerIP" ]]; then
  echo "Enter the Apple TV address: scripts/atv/spectrum/atvpair.sh <Apple TV address>" >&2
  exit 2
fi

configDir=/root/.android
configFile="$configDir/.pyatv.conf"
mkdir -p "$configDir"
pairingDir=$(mktemp -d "$configDir/.pyatv-pair.XXXXXX")
pairingFile="$pairingDir/.pyatv.conf"
trap 'rm -rf "$pairingDir"' EXIT

if [[ -f "$configFile" ]]; then
  cp -- "$configFile" "$pairingFile"
  chmod 600 "$pairingFile"
fi

echo "Apple TV will show a PIN. Enter that PIN here when asked."
atvremote --storage-filename "$pairingFile" -s "$streamerIP" --protocol companion pair

if [[ ! -s "$pairingFile" ]]; then
  echo "Pairing did not create an Apple TV configuration; the existing pairing was left unchanged." >&2
  exit 1
fi

chmod 600 "$pairingFile"
mv -f "$pairingFile" "$configFile"
echo "Apple TV pairing saved. Return to Settings and check the connection again."
