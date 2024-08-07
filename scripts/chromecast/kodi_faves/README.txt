A tuner for kodi favourites list

INTRODUCTION

kodi has a huge number of add-ons for various types of live
streaming. We can use kodi for all the hard work of finding and
decoding the streams (including DRM sometimes) and just run its player
output through an HDMI encoder to capture it. Even if there is a
dedicated app for particular content, kodi might be a better choice
for scripting because it has a fairly rich API and doesn't depend on
quirky adb remote control emulation with finicky delays.

PREPARATION

* Obviously, install kodi.

* Install whatever kodi add-ons you want to deal with live streams.

* For any stream you want to treat as a "channel", add it to kodi "Favourites"
  (methods for that might vary with different kodi add-ons, but typically
  it will be some kind of context menu with "add to favourites" or similar).

* Test manually to see that you can actually play all the desired stuff in kodi.

* Also check that anything you select from the "Favourites" screen starts playing
  with no further input or interaction. Pop-ups or other confirmation things
  are OK if they automatically go away within a short amount of time.

* If you plan to use "favourites navigation" to tune channels (which
  is not the default option), then from the "Favourites" window,
  select "Options" and use a ViewType of "WideList". That's important
  because we have to move around in the list as if we were using arrow
  keys, and the scripts only move down or up, not left or right.

* Enable the kodi JSONRPC API via HTTP.
    - Settings > Services > Control
    - Set or edit username and password
      (imagine me giving you a patronizing lecture about protecting these credentials)
      (but, seriously, be secure)
    - Allow remote control via HTTP
    - The default port is 8080, but you can change it if you want to or need to
    - Allow remote control from applications on other systems
    - Enable SSL if desired (see kodi docs)

* In the ah4c environment, you must configure the kodi password that you created
  just above. See the configuration stuff below.

* If you are running kodi on Android, you must configure adb access
  from the ah4c server machine. If you are running kodi on Linux, you
  must configure ssh access from the ah4c server machine to the box
  running kodi. Actually, if you don't attempt to do device
  sleep/wakeup, force stop, or reboot with the Linux flavor, you
  probably don't need to configure anything for the SSH access at
  all. That's probably the case for you. You can just give things a
  try and look for messages about failed SSH operations in the ah4c
  log. If you need it, you are creating an SSH key for the Linux
  account that runs kodi. For LibreELEC, that account is root. The SSH
  key is NOT for the JSONRPC account. (If you don't know about SSH
  keys, this article can help you work through it:
  https://www.digitalocean.com/community/tutorials/how-to-set-up-ssh-keys-2)

NOTE: kodi's spelling is "favourites", not "favorites". That's the
spelling you should use except as noted specifically below. Also, the
internal name for what you see as "Favourites" in the GUI is actually
"favouritesbrowser". We accept either "favourites" or "favorites" in
the channel tuning info in an attempt to be less confusing.

* TUNING AND STATION SELECTION

The channel tuning info (the final part of the URLs in the m3u) should be:

  Waaa_favourites_Some%20Unique%20Label

That's three chunks separated by underscores.

The first part ("Waaa") can be anything you want to remind
yourself. It's otherwise ignored by the scripts.

The second part is literally "favourites". The scripts check for that
and reject any request that doesn't match exactly. The reason for that
is in case there is someday some other kind of kodi tuner that might
share the script code. As a special favor (:-)) to Americans, you can
use "favorites" (without the "u") as an acceptable alias.

The last part, the "tuning hint", is a URL-encoded string that must
match an item in your favourites list. (Although you have to
URL-encode it in your m3u, it's already been URL-decoded by the time
it gets to these scripts.)  Here."match" means it's either a complete
or substring match done case insensitively. For example, "Foo%20Bar"
would match a favorite channel with the label "My foo barlicious
Channel". In the case of multiple matches in your favourites list, you
don't really have much control over which one will be selected, so be
as specific as possible. In the unlikely event that you have actual
ambiguities in your favourites list, use the kodi favourites context
menu to rename some of them to ensure uniqueness.

How do you know what the label is for a favourites item? It's not
necessarily the same as whatever text appears in the logo or
thumbnail. However, in the "WideList" view, it will be the same as the
text displayed beside the logo or thumbnail. You can look in the
scripts to see how it finds the labels, but the simplest way for you
to be absolutely sure is to use the context menu to attempt to rename
the item. That will show you the current label, and you can just
cancel out of that after you have made a note of it.

Don't use literal underscores other than to separate the 3 pieces as
illustrated. And, except as just described, don't use any characters
that may cause trouble in a URL. Even with URL-encoding, avoid
anything special to Linux shells (single or double quotes, dollar
signs, vertical bars, etc) in the tuning hint portion.

* SCRIPTS STRUCTURE AND CONFIGURATION

Our caller, ah4c, expects there to exist 3 scripts: "prebmitune.sh",
"bmitune.sh", and "stopbmitune.sh". For this set of scripts, there are
several common definitions, functions, etc. Each of the expected
scripts is a trivial 4-liner that sets the FLAVOR, sources "common.sh"
and then calls the applicable function defined within "common.sh". The
intent of that arrangement is to achieve more consistent naming,
simpler editing, and so on. All of the scripts redirect stdout to be
on top of stderr.

There is a 95%-plus overlap between the kodi scripts running on
Android TV and kodi running on Linux. The top-level scripts
set ane environment variable "FLAVOR" to indicate which flavor is
being used, ("android" or "linux") and "common.sh" has conditional
logic in the applicable places. There is only one copy of
"common.sh". It's located under scripts/chromecast/kodi_faves/ because
that's where it first appeared.

At the top of "common.sh" is a collection of variables whose names
start with "CONFIG_". As you might guess, those are things that
conditionally control aspects of the script behaviors. If you are
happy with the default values defined in "common.sh", then that's all
you need to know. If you want to change any of them, you can, of
course, just modify "common.sh".  A better way is to create a file in
the same directory (as common.sh) called "config-local.sh" and provide
modified values for just the items of interest. Copy/paste/modify is
the most reliable way of doing that. The advantage of using
"config-local.sh" is that your changes would not be overwritten by any
updates to this set of scripts. One way or the other, you will have to
at least configure the password for JSONRPC API access. If you are
running kodi on Linux, you will have to configure the ssh key.

You can use the FLAVOR variable to put conditional logic into your
"config-local.sh" file. The possible values are "android" and "linux".

The scripts use a tool called "jq" for parsing JSON responses. jq
is not present in the ah4c docker image, so the scripts detect that
and install it if necessary. It's a fairly small package.

* DELAYS

The scripts mostly avoid doing what's called "remote emulation".  So,
configurable delays are not too critical, and there are only a few.
Those delays are a bit fiddly and depend on your specific equipment,
what your equipment is doing at the moment, and, in some cases, the
response time to network requests over the internet. Phase of the moon
might be in there somewhere.

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
instead of the first minute of the real program. For some shows, that
might not matter if they have advertising or recaps or whatever at the
beginning. However, it's best to not rely on that and have the delays
as short as you can and still have them tune reliably.

* BACKSTORY

(You can stop reading now.)

I got involved in this whole ah4c business because of a single local
PBS station that I can't pick up with my antenna. I started out by
using the PBS app with remote control emulation an an Android TV
dongle. That works, modulo the occasional loss-of-marbles, but it's
fiddly at best. I also experimented with a different technique called
VLC-bridge-PBS. That works really, really well, but I didn't find a
way to enable closed captions. Sources for VLC-bridge-PBS don't seem
to be available. In poking around, I saw that it's using a kodi plugin
to handle DRM. That's what got me heading down the path of using kodi
itself to "tune" the PBS station. kodi's rich API makes moving around
the GUI pretty straightforward for most things, and that eliminates a
lot of fiddliness.

After I got the kodi stuff working reasonably well on my Android TV
dongles, I decided to see what my collection of ancient Raspberry Pi
boards could do for me. After some experimenting, I settled on using
LibreELEC, which is available in the standard Raspberry Pi imager
tool. I run that on a Raspberry Pi 3 with 1 GB RAM, booted from a 16
GB thumb drive.

Pro tip: If you are having problems with getting kodi to remember that
you always want subtitles, you are not alone. This thread may help you
(it helped me): https://www.reddit.com/r/kodi/comments/rrwkko/comment/hqshh81
