Tunes live TV in the PBS app.

* PBS STREAMING INFRASTRUCTURE

Most PBS affiliates use the streaming platform operated by the
national PBS umbrella organization, and that's what the PBS app
is for. I think there are a few PBS affiliates that use some
different infrastructure for streaming, and you won't be able
to use the PBS app and these scripts for those affiliates.

The scripts here are aimed at navigating to a PBS affiliate's
live stream. They don't know anything about on-demand programming,
even though it is available in the same PBS app.

* SCRIPTS STRUCTURE AND CONFIGURATION

Our caller, ah4c, expects there to exist 3 scripts: "prebmitune.sh",
"bmitune.sh", and "stopbmitune.sh". For this set of scripts, there are
several common definitions, functions, etc.  Each of the expected
scripts is a trivial 3-liner that sources "common.sh" and then calls
the applicable function defined within "common.sh". The intent of that
arrangement is to achive more consistent naming, simpler editing, and
so on.

At the top of "common.sh" is a collection of variables whose names
start with "CONFIG_". As you might guess, those are things that
conditionally control aspects of the scripts behaviors. If you are
happy with the default values defined in "common.sh", then that's all
you need to know. If you want to change any of them, you can, of
course, just modify "common.sh".  A better way is to create a file in
this same directory called "config-local.sh" and provide modified
values for just the items of interest. Copy/paste/modify is the most
reliable way of doing that. The advantage of using "config-local.sh"
is that your changes would not be overwritten by any updates to this
set of scripts. If you don't want to modify any config values, it is
not necessary to create "config-local.sh" at all.

* TUNING AND STATION SELECTION

The channel tuning info (the final part of the URLs in the m3u) can be
one of two forms:

"Waaa"
  or
"Waaa_12345_2".

In both cases, the tag "Waaa" (or whatever) is ignored.  It's just
documentation for you, our dear user. Station call sign or marketing
names are good choices. For the second form, the separator character
is underscore, so don't include any underscores in the tag. For URL
reasons, don't include any slashes or spaces or other URL unsafe
characters in any part of the channel tuning info.

For the first form, live TV is selected with whatever local PBS
channel you have last configured in the app. This is most useful for
the majority of places which are only served by a single PBS affiliate
(or if you only ever watch a single affiliate).

The second form has a ZIP code used to populate the app's search box
for stations. The last number is a one-based position in the results
list. I'm hoping the results always come back in the same order, but I
have no way to verify that. This is most useful for places served by 2
or more PBS affiliates. There are many places with 2 and a few places
with 3. I don't know if there are any places with more than 3.

For example, in the Seattle and Tacoma, Washington, area, there are 2
PBS affiliates. If you search for a Seattle area ZIP code, you get 2
results back. So far, I've always seen them come back as KCTS (the
Seattle affiliate) first and KBTC (the Tacoma affiliate) second. You
select which one you want with either "_1" or "_2". You can see this
in the sample M3U pbs-seatac.m3u. There's another example M3U,
pbs-worcester.m3u, for a ZIP code in Worcester, Massachusetts. That
ZIP code search offers a choice of 3 PBS affiliates.

The tuning script remembers the last station it tuned, so it only goes
through the station search dialog if it needs to change to a different
PBS affiliate (or if a lot of time has passed).

NOTE: Although the search for affiliates is based on the ZIP code
entered in the search box, the PBS streaming platform does geographic
restrictions based on the source IP address it sees. That's why you
can't watch out-of-area PBS programming. The PBS app will let you
search for any ZIP code and will display the results. Selecting an
out-of-area PBS affiliate in the search results will give an error
message. The code in these scripts doesn't have a way of knowing that
and assumes the station selection went correctly. There's also a small
chance that your ISP's location will be different from your location
for the purposes of PBS restrctions. If you can select it manually in
the PBS app, the scripts can select it.

* DELAYS

The scripts work mostly by doing what's called "remote emulation".
That is, they use adb commands to simulate button presses on a remote
control. When a human operates the remote, it's obvious when they can
press more buttons. The scripts mostly don't know when a reaction has
happened on the screen, so they resort to a bunch of fixed delays to
wait until the right time for the next thing. Those delays are a bit
fiddly and depend on your specific equipment, what your equipment is
doing at the moment, and, in some cases, the response time to network
requests over the internet. Phase of the moon might be in there
somewhere.

Delays are expressed in seconds and need not be limited to whole
numbers. Decimal fraction amounts are acceptable, e.g., "1.345".

Instead of calling sleep directly, the scripts use the "settle"
function (defined in "common.sh") which scales how much they
sleep. When called, that function alters the requested sleep duration
according to CONFIG_DELAY_SCALING and CONFIG_DELAY_OFFSET.  If the
scaling is 1 and the offset is 0 (the defaults), you get exactly the
sleep durations coded into the scripts. If you need proportionally
longer delays, increase the scaling. If you want proportionally
shorter delays, decrease the scaling.  If you want to add (or
subtract) a fixed amount to each delay, set the offset to that amount.
If you configure the scaling and offset values to 0, sleep becomes a
no-op (which probably will not be a good choice).

There are CONFIG_ items for each individual delay. Instead of scaling
or offsetting them across the board, you can adjust them individually.

The advantage of long delays is that you can be sure your device has
finished reacting to inputs before the script moves on to the next
thing.

The disadvantage of having overly long delays is that you risk losing
the beginning of real programming. For example, if you are recording a
30 minute program and you have 1 minute worth of delays, then the
first minute of the recording will be showing this tuning noise
instead of the first minute of the real program. For PBS shows, that
usually doesn't matter since they tend to thank sponsors at the
beginning and sometimes show a preview of the show. However, it's best
to not rely on that and have the delays as short as you can and still
have them tune reliably.  Timing is trickiest if you have multiple PBS
affiliates and need to change your local station before recording.

In my environment, with the default delays, it takes about 30 seconds
to tune when a station change is required. It takes about 12 seconds
when a channel change is not required. Tested on Google Chromecast HD,
Tivo Stream 4k, and Onn 2k Stick. I also tested against a fairly old
Amazon Fire TV stick, though I had to scale the delays a bit for that
to work.
