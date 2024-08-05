A tuner for kodi favourites list

This set of scripts shares commonality with the scripts under
scripts/chromecast/kodi_faves/. See the README.txt under there for
important details, configuration options, etc.

This should work on any sort of Linux box that can run kodi. I tested
it with a few distributions before I settled on one called
plain old raspbian. I tried it on a few differnt Raspberry Pi boards that I had
on hand.

On a couple of RPi 1 boards, the lag and stutter was pretty bad. On an
RPi 2 board with 1 GB of RAM, playing was mostly smooth but with
occasional lag and stutter. On an RPi 3B+ with 1 GB of RAM, playback
was completely smooth for 1080p/60fps. An 8gb microSD card is way more
than enough to hold the image and kodi addons. Of course, you have to
decide if running from a microSD card is too risky. If you run from a
USB thumb drive, navigation may be a bit sluggish but playback should
be unaffected.
