#!/bin/bash
# atvpair.sh for atv/spectrum
# 2026.08.05

#docker exec -it ah4c scripts/atv/spectrum/atvpair.sh <appletv_ip>

streamerIP="$1"

mv /root/.android/.pyatv.conf /root 2>/dev/null || echo "No existing .pyatv.conf found"

atvremote -s $streamerIP --protocol companion pair

mv /root/.pyatv.conf /root/.android && echo ".pyatv.conf moved to persistent directory"
