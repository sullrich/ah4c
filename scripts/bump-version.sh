#!/bin/sh
# bump-version.sh — stamp VERSION with the build time, in UTC.
#
# CalVer, so the version says when rather than pretending to say what:
# vYYYY.MM.DD.HHMM. UTC and not local time, because a build stamped in two
# timezones sorts wrongly and nobody can tell which one they are holding.
#
# Every field is fixed width, so a plain string compare orders them. If the
# stamp is not strictly newer than what is already in the file — two builds in
# the same minute, or a clock that went backwards — the last field is
# incremented instead, so the version never repeats and never goes down.
set -eu
cd "$(dirname "$0")/.."
now="v$(date -u +%Y.%m.%d.%H%M)"
cur="$(cat VERSION 2>/dev/null || echo)"
if [ -n "$cur" ] && [ "$now" != "$(printf '%s\n%s\n' "$now" "$cur" | sort | tail -1)" ]; then
	now="$(printf '%s' "$cur" | awk -F. '{printf "%s.%s.%s.%04d", $1, $2, $3, $4 + 1}')"
elif [ "$now" = "$cur" ]; then
	now="$(printf '%s' "$cur" | awk -F. '{printf "%s.%s.%s.%04d", $1, $2, $3, $4 + 1}')"
fi
printf '%s\n' "$now" > VERSION
echo "$now"
