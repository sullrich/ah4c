# ah4c

### This is a fork of https://github.com/tmm1/androidhdmi-for-channels with these features:

0. ENV variable support
1. Standardize and improve script durability / reliability
2. Allow multiple tuners from one set of scripts
3. Allowing the tuner and encoder information to be dynamically set.  Useful for docker containers, etc
4. Support for FireTV and Hulu
5. Test each pre script and if fails move on to next tuner before giving up
6. M3U file serving with templating for IP Address
7. Docker support
8. Application based tuners (IE: magewell, hauppauge colossus 2 & anything ffmpeg supports!)
9. E-Mail alerts on failures
10. Global logging to disk with rotation
11. Logging endpoint /logs for moments you do not have access to console with dynamic refresh!
12. Webhook support on failure use $reason variable in URL.
13. Custom script support - drop in your scripts and set STREAMER_APP env variable to match dir location
14. Web graphs of cpu, mem, gpu (nvidia)
15. Tee support (sending feed to a secondary target)
16. Application based tuning! Just send the feed to stdout
17. Dead video feeds restart - video locking up but audio working
18. Use OCR if tesseract is installed looking for common questions such as Whos there? and Still watching?
19. NULL packet insertion - fills encoder stalls with MPEG-TS NULL packets (PID 0x1FFF) so the DVR sees a continuous bitstream during HDMI source gaps
20. Closed captions - live CPU speech-to-text written into the stream as CEA-608, the way an HDHomeRun carries them, with no re-encode and nothing added to the image
21. Autocrop for Xfinity channels with black borders on all 4 sides, driven live through a LinkPi Encoder's web API
22. Custom startup script - run your own script alongside ah4c at container start via USER_SCRIPT
23. Tune hold - PLAYBACK_DELAY holds every tune from the request while a slow app reaches its video, opening the wait with a moment of black and carrying the rest of it on packets the DVR cannot put on a timeline, so a tune longer than the DVR's 30 seconds still records
24. Pre-roll - bind-mount a video or still image of your choice and it is shown instead of NULL packets while a tune is held or an encoder stalls

ah4c WebUI:

<img width="1666" height="844" alt="Screenshot 2026-08-23 at 11-03-06 ah4c - Organizr V2" src="https://github.com/user-attachments/assets/7859428d-6430-4025-8d2e-e5181f9dd970" />

### Activity:

<img width="1920" height="2083" alt="screencapture-docker6-2026-08-19-19_09_55" src="https://github.com/user-attachments/assets/1e221fe5-cf23-48b6-81db-e733552ba4e7" />

(built in stats gui)

### M3U Editor

<img width="1666" height="844" alt="Screenshot 2026-08-23 at 11-13-34 ah4c - Organizr V2" src="https://github.com/user-attachments/assets/241ff599-2573-4281-8a61-b84519754503" />

### Closed Captions

<img width="1666" height="2823" alt="screencapture-docker6-2026-08-23-11_15_39" src="https://github.com/user-attachments/assets/65fccf8a-9f85-4723-8e4b-c0e0b876cd6f" />

Streaming apps hand the encoder a picture with the captions already stripped off, so
everything downstream of ah4c has nothing to display. The **Closed Captions** page adds
them back.

> **One volume has to be added before you use this.** The speech model and everything else
> is downloaded on demand, and without somewhere on the host to put it, all of it lives
> inside the container and is thrown away the moment the container is recreated. Add this
> to the `ah4c` service and recreate it:
>
> ```yaml
>       - ${HOST_DIR}/ah4c/captions:/opt/captions
> ```
>
> ah4c checks for this at startup, says so in the log, and puts a warning at the top of the
> Closed Captions page if it is missing, so you will not lose a download without being told.
> Nothing else about the image or the compose file changes, and the directory stays empty
> unless captions are switched on.

Audio is pulled out of the encoder's transport stream, transcribed by a speech model
running on this machine, and written back into the video as CEA-608 caption data carried
in ATSC A/53 user data — the same carriage an HDHomeRun uses for over-the-air captions.
Channels DVR, VLC and anything else see a real closed caption track and offer it under
the usual subtitles button. Nothing is sent anywhere: no account, no API key, no audio
leaving the box.

- **Not burned in, and not re-encoded.** The compressed video is passed through
  untouched; only a small caption message is inserted ahead of each picture, and the
  picture data comes out byte-for-byte identical. Quality, bitrate and tune time are
  unchanged, and streams that already carry captions are left alone.
- **Nothing is added to the Docker image.** No new packages, no Python, no model.
- **ah4c stays pure Go.** Recognition runs against
  [transcribe.cpp](https://github.com/handy-computer/transcribe.cpp), a ggml engine opened
  at run time with purego. There is no cgo and nothing linked into the binary —
  `CGO_ENABLED=0` still builds.
- **Nothing is gated on an environment variable.** Everything is controlled from the web
  UI and stored in `captions/config.json`. Changes apply to the next tune.
- **No GPU, no /dev/dri, no hardware encoder.** Nothing here requires a graphics card:
  everything but the high-end choice runs faster than real time on an ordinary CPU, and
  that one is labeled for what it wants. The backend is chosen automatically — the best build
  this container can actually load — and the log says which one each model really runs on.
- **Entirely opt-in.** With captions off, a tune takes exactly the path it always did.

The page offers an engine plus your choice of model. Nothing is bundled and nothing is
fetched until you ask for it. Every download URL is shown on the page next to the button,
so it is always clear what is being fetched and from where.

**Speech engine** (required) — one program, [transcribe.cpp](https://github.com/handy-computer/transcribe.cpp),
runs every model. It is downloaded once from that project's GitHub releases — about 28 MB
for the build that covers both the processor and Vulkan, 216 MB for CUDA — and every model
uses the same copy. The backend is picked automatically (the best one this container can
load), a picker exists if you want to pin it, and the log states which backend each model
actually runs on.

| Build | Needs |
| --- | --- |
| CPU + Vulkan (one download) | Nothing for the processor; a Vulkan driver and `/dev/dri` for the GPU |
| CUDA | The NVIDIA container runtime; the download carries its own CUDA runtime |

**Vulkan** covers Intel and AMD graphics. The driver is not in the image, so the page
downloads it the same way it downloads a model: the packages and everything they depend on
are saved into `captions/drivers` inside the bind mount, and put back automatically
at startup after a rebuild, without needing the network again.

The driver package also ships llvmpipe, a Vulkan device that is really the processor in
disguise, and the loader quietly offers it whenever the real driver cannot reach the card —
which looks like a working GPU and runs three times slower than the processor used
honestly. ah4c refuses it: only hardware Vulkan drivers are considered, the log prints the
name of the device actually doing the work, and if no real card is reachable the engine
runs on the processor and says so.

You also have to pass the graphics device through, which is off by default. Set this in your
env file and recreate the container:

```
GPU_DEVICE=/dev/dri
```

**CUDA** needs nothing installed: the download carries its own CUDA runtime, and the NVIDIA
container toolkit supplies the driver. Set these instead:

```
DOCKER_RUNTIME=nvidia
NVIDIA_VISIBLE_DEVICES=all
NVIDIA_DRIVER_CAPABILITIES=compute,utility
```

Both are ordinary env settings rather than edits to the compose file, and both default to
off: `GPU_DEVICE` passes `/dev/null`, which exists everywhere and does nothing, and an empty
`NVIDIA_VISIBLE_DEVICES` exposes no GPU. Nobody without a card has to change anything.

A GPU build is grayed out until the library it needs is loadable, which is settled by asking
the dynamic loader rather than by guessing. If a GPU build is selected but cannot run, the
engine falls back to the processor rather than failing, so captions keep working. Apple
silicon gets Metal in its single build and has no choice to make; arm64 Linux has no CUDA
build from either project and is offered CPU and Vulkan.

Quick Sync is not on that list and cannot be. It is fixed-function video encode and decode
hardware, not a compute unit, so nothing can run a model on it. The VA-API packages already
in the image are for video and are unrelated.

**Models** — three. Cohere Transcribe is the most accurate thing there is in the eight
languages it reads. Nemotron reads thirty-two, transcribes as the audio arrives rather
than a phrase at a time, and on a GPU keeps up with Cohere while landing a second behind
the picture instead of four — so it is the one to pick for anything that is not English,
and a fair choice even where it is. Moonshine is for hardware that cannot run either.
Every recommendation is guidance, never a gate, and all three run anywhere.

| Model | For | Accuracy | Delay | Download | Languages |
| --- | --- | --- | --- | --- | --- |
| **Cohere Transcribe 03-2026** — the most accurate open model there is | English and seven others. Runs on the processor; a GPU takes the load off it | Best — 1.3% | Two to four seconds, like broadcast | 1.6 GB | 8 |
| **Nemotron 3.5 ASR Streaming 0.6B** — thirty-two languages, transcribes as the audio arrives | Anything not in English, and anyone the delay bothers. Keeps up with Cohere on a GPU; costs memory per tuner | Very good | About a second | 496 MB | 32 |
| **Moonshine Streaming Tiny** — forty-eight megabytes, streams live | Very small machines: a Celeron, a low-power NAS, a Pi | Decent — 4.5% | Under a second | 48 MB | English |

Accuracy is word error rate on LibriSpeech test-clean; read it as a ranking, since live
television is harder than clean read speech for every model.

The thing to understand before choosing is that phrase models and streaming models are
different shapes, not fast and slow versions of one thing. A phrase model waits for a
sentence to finish and then transcribes all of it, so it has the whole phrase to reason
over and cannot be quicker than the phrase is long. A streaming model keeps a running
state and commits words as they settle, so it is about a second behind whatever is being
said and can never see what comes next. That is where the accuracy difference comes from,
and it is why no setting closes the gap in either direction.

**Memory**, and this is where the choice bites hardest. A phrase model is loaded once and
shared: Cohere Transcribe costs about 2.2 GB resident however many tuners are captioning,
and it is freed when the last of them ends. A streaming model cannot be shared, because
the running state that makes it streaming belongs to one stream — so every captioned tuner
loads its own copy. Nemotron is roughly 500 MB per stream, which is comfortable for three
and five gigabytes across ten; Moonshine is about 160 MB per stream. The page works the
total out for your own setup, but the shape of it is worth knowing before you pick: the
accurate model gets cheaper per tuner and the quick ones get dearer.

One copy is all Cohere loads, however many tuners are on it. If it cannot keep pace with the
streams feeding it, captions thin themselves to stay current and the log says so — memory
is never spent to cover for a slow setting on its own.

**Whether the recognizer is keeping up** is measured and shown on the Closed Captions page,
under the switch:

> Transcribing at **4.6× real time** on Vulkan, with 4 streams captioned. Phrases wait
> **0.31s** to be transcribed.

The wait is the figure that answers the question. It is the gap between a phrase being cut
out of the audio and being transcribed: under a second is keeping up, and climbing is not.

The real-time factor beside it is seconds of audio transcribed per second of compute — a
property of the model, the quantization, the backend and the machine. It is **not** a number
of streams, and this used to claim it was. Two things are wrong with that: only speech is
ever queued, so a captioned stream does not submit a second of audio for every second it
runs, and the factor does not move with the stream count anyway — four streams measure the
same 4.6× as one. Read the wait for capacity and the factor for how fast the hardware is.

The same figures go to the log every hundred dispatches, for a record over time:

```
[CC] recognizer: 4.6x real time, 1.4 phrases per dispatch, 0.61s compute for 2.8s of audio,
     phrases waited 0.31s, 0 in the queue, 4 streams captioned — over the last 100 dispatches
```

**One recognizer, and there is no setting for it.** There was one — up to eight copies of
the weights, chosen on the page — and it is gone because it was measured and it made
transcription slower every time it was raised. Not slower per copy: slower outright,
further behind real time with four than with one.

Three reasons, all pushing the same way. Batching is the largest: the engine runs several
phrases in a single dispatch much faster than the same phrases one at a time, and splitting
the queue across copies meant nothing ever batched — the `recognizer split` line in the log
prints both figures side by side and the gap is not small. Threads are the second: the
thread allowance is a figure for the machine, not for a copy, and every copy took all of
it, so four copies asked for four times the cores the machine has — and the engine
spin-waits its threads rather than sleeping them, so they fought each other and the tuners
for the same cores instead of interleaving politely. The device is the third: one graphics
chip does one piece of arithmetic at a time however much is queued on it, and each copy
holds its own 2.2 GB of weights in the same system memory an integrated chip reads through.

So the queue has one server, it takes every phrase waiting, and it batches them. When it
cannot keep pace the freshness rules thin the phrases to stay current and the log says so.
Memory is never spent to cover for it, because spending memory did not help.

Nothing is loaded until a tune is already playing, so captions can delay themselves but
never a tune; a start that fails says why in the log and retries while the stream plays.

**Timing** is not a setting: each model runs at the operating point it prefers. Cohere
Transcribe reads phrases of up to four seconds and lands captions two to four seconds
behind the picture — what live broadcast captioning runs. Nemotron and Moonshine stream at
their own trained cadences and land about a second behind. The recognizer reports its throughput in
the log, so whether your hardware is keeping up is a measurement, not a guess. The display
is paced too, by two settings. **Reading speed** is how fast words are let onto the screen,
in words a minute: the channel could push sixty characters a second and nobody speaks at a
quarter of that, so without a pace a sentence lands all at once and the screen sits idle
until the next one. Captioning guidance puts subtitle speed between 120 and 160 words a
minute and the whole range is offered, 150 by default. **Time on screen** is the least time
a line stays readable before it is allowed to leave, two to eight seconds. A roll-up does
not put lines up together — each row is added and the oldest scrolls away — so the figure is
shared out between the rows above it, and more rows means each roll waits less. Neither can
go below the guidance minimum of a second for one line, a second and a half for two and two
for three, whatever is chosen.

Captions are rendered in capitals, which is the long-standing convention for broadcast
captioning and is easier to read across a room; there is a setting for mixed case. That
also evens out the streaming models, which write in lower case.

The right build for the machine is chosen automatically, so the arm64 image fetches the
arm64 engine without being told.

Both engine and model land in `/opt/captions`, which is why that volume has to exist. On
the host they sit in `${HOST_DIR}/ah4c/captions`, beside the `scripts`, `m3u` and `adb`
directories ah4c already keeps there. Remove either download from the page to reclaim the
space.

How far behind the captions run depends on the model. A streaming one transcribes as the
audio arrives and lands about a second back; a phrase-at-a-time one has to wait for the
sentence to finish and lands three or four seconds back. Either way it is the same kind of
lag live broadcast captioning has. An optional extra delay is available if you want to push
them back further.


### Tune hold and pre-roll

Channels DVR gives a tune about thirty seconds to deliver its first bytes. An app that
takes longer to reach its video — some cable apps do — loses the recording to the next
tuner, or fails outright. A variable and a bind mount deal with that; both apply to
network encoder tuners only, and neither changes a tune that has nothing holding it.

**`PLAYBACK_DELAY`** holds every tune for a set time from the request, then starts the
program. Any bare number is seconds — `15`, `30`, `0.5` — otherwise a duration like `30s`
or `1m`. **Ten minutes is the ceiling**, and anything longer is held for ten minutes with a
line in the log saying so. That ceiling is a guard against a typo rather than a measured
limit: forty-five seconds is the longest hold that has been watched land the viewer at the
live edge, and longer ones are still being tested, so treat anything much past it as
untried. The value is the whole tune, scripts included:
the DVR is answered the moment it asks, before the pre script has woken the box, so the
seconds the scripts take belong to the hold instead of piling up in front of the program.
Set it to a little more than the app needs from the moment the DVR asks to the moment its
picture is up.

A hold has to cost the viewer as little as it can, and that is the hard part. Nothing of
the box tuning in may pass — no app screens, no half-drawn menus, no sound — because
anything sent during the wait becomes part of the recording and stands in front of the
program.

Most of the wait is MPEG-TS NULL packets — PID 0x1FFF, the stuffing a transport stream
already carries to fill spare bandwidth. They hold no picture, no sound and no clock, so a
DVR that stores every byte of them still has nothing it can put on a timeline. They go out
on a diet: enough while the DVR is deciding the body really is a stream, a keepalive once
it has decided, because every byte sent during a hold is a byte the DVR is holding ahead of
the show. The filler stops altogether for the last second before the hand-off, so the
stretch directly in front of the program is not filler at all.

Not all of it, though. A player given nothing but stuffing has no time base to anchor on
and a playhead cannot get past it, where the same stretch of NULL packets alongside a
stream that has shown a picture is carried across without trouble. So a two second clip of
black video and silence is generated with ffmpeg once at container start, before the
listener is even bound, and looped for about a second of the wait — long enough for the
player to take a picture and a time base from it, short enough that the recording is not
carrying tens of megabytes of black. NULL packets carry the rest. If ffmpeg cannot make
the clip the log says so under `[BLACK]` and the whole wait is NULL packets, exactly as it
was before. Where in the wait that second of black sits is still being worked on, so read
the `[BLACK]` lines rather than this paragraph for what a given build does.

The encoder is opened at the start of the tune and read for the whole wait, and everything
it produces before the delay is up is thrown away. That is what the rest of this program
already does — the stall-tolerant reader fills gaps in a stream it is still reading, and
playback detection drains the encoder until the app is seen playing — and it is the one
thing this hold used to do differently: it left the encoder shut and opened it cold at the
end. When the delay passes, the gate takes the first keyframe out of a connection that has
been up and flowing the whole time, so the program starts on a whole picture at the
encoder's live edge rather than at the top of a session that still has to be established.
Anything the encoder had queued from before is read off and thrown away first, and any
stuffing left in the gate's first release is stripped out, so the picture is the first
thing the DVR sees after the wait.

With a pre-roll mounted the hold works the older way: the pre-roll plays, the encoder is
opened when the wait ends, and the first packet of each stream is then marked with the
transport stream's own discontinuity indicator, so the DVR is told the clock it is about to
see is a new one. The drained path deliberately does not mark. Marking there splits the
streams apart — the player is told the video's time base is new and flushes it — and fast
forward, which navigates by video keyframes, finds none in the stretch while the video
re-anchors and stops instead of moving.

In earlier builds this variable skipped the start of the tune through ffmpeg and was
capped below the DVR's 30 seconds. That cap went with the mechanism: nothing sits between
the encoder and the DVR while a recording runs, and ffmpeg is asked for nothing at tune
time — the black clip is made once, at container start. The log prints all of it under
`[HOLD]` and `[BLACK]`.

**The pre-roll** is a video or still image shown to the DVR instead of NULL packets,
anywhere they would otherwise go: a tune held by `PLAYBACK_DELAY` or `PLAYBACK_DETECTION`,
and an encoder stall covered by `NULL_FRAME_INSERTION`. Anything ffmpeg reads works. It is
not a setting inside the container but a bind mount at `/opt/preroll`. Either drop the file
into `${HOST_DIR}/ah4c/preroll` on the host, which sits beside the `scripts`, `m3u`, `adb`
and `captions` directories ah4c already keeps there and is mounted by default, or set
`PREROLL_FILE` to the file's path anywhere on the host and that file is mounted instead:

```
PREROLL_FILE=/data/preroll.mp4
```

```yaml
      - ${PREROLL_FILE:-${HOST_DIR}/ah4c/preroll}:/opt/preroll
```

With more than one file in the directory, one named `preroll.*` is used; otherwise the
first by name, and the log says which.

The file is prepared once, at container start, into a transport stream: a video whose
codecs already suit one (H.264, HEVC or MPEG-2 with AAC, AC3, MP2 or MP3) is remuxed as it
is, anything else is encoded to H.264 and AAC, a still image becomes a ten second clip with
silent audio, and a silent video gains silent audio. It loops for as long as the wait lasts
and stops the moment the real stream is ready. It is the first thing the DVR gets: the
request is answered with the pre-roll already playing, before the pre script has woken the
box, and the tune runs underneath. With nothing holding the tune, the encoder takes over
the instant its stream is up and the pre-roll simply stops, wherever it was; with
`PLAYBACK_DELAY` or `PLAYBACK_DETECTION`, the same pre-roll carries on under the hold. A
new file needs a container restart, or recreating it when `PREROLL_FILE` names the file,
since then the mount is of the file itself. The splice is cleanest when the pre-roll matches the encoder's resolution and codecs. If the
file cannot be prepared the log says why under `[PREROLL]` and the tune falls back to NULL
packets, so a pre-roll can never cost a recording.

The wait is always as long as the hold, never as long as the pre-roll happens to be. A
clip shorter than the hold repeats until the hold is over — a still image simply stays up
— and a clip longer than the hold is cut off when the hold ends. The hold is
`PLAYBACK_DELAY`, or ten minutes if that asks for more.

The pre-roll and the program share one video stream so the player never has to switch
tracks mid-recording, which is the one thing it will not do. One consequence is cosmetic
and worth knowing: a player's stats overlay reads its **specified** frame rate from the
first picture it sees, which is the pre-roll's, and keeps showing that number after the
program takes over. So a 30 fps pre-roll in front of a 60 fps channel leaves the overlay
reading `29.970 (specified)` for the whole recording. This is a label only. The program is
carried and played at its own rate — the same overlay's **estimated** figure reflects it,
dropped frames stay at zero, and audio and video stay in sync. Measured on a real tune: the
program ran 1076 coded frames across 17.9 seconds, 60.00 fps, every frame exactly 1/60th of
a second apart on the transport clock, while the overlay still said 29.970 specified.

**An H.265 encoder** needs one setting: `ENCODER_CODEC=h265`, and the **hold** — `PLAYBACK_DELAY`
or `PLAYBACK_DETECTION` — is fully supported. The player will not switch its video decoder
mid-recording, so the black played inside the hand-off has to be the program's own codec, or
the picture freezes on it instead of crossing to the program. This image's ffmpeg has no
software H.265 encoder, so ah4c cannot make an H.265 black at startup; a half-second clip is
built once, offline, and shipped inside the binary, and `ENCODER_CODEC=h265` selects it.

The **pre-roll** is the exception. It is the first coded video the player sees, so it must
itself be H.265 — an H.264 pre-roll would lock the player onto an H.264 decoder that then
cannot cross into the H.265 program (nothing bridges that: not a NULL gap, not a black seam,
not a relabel; all were tried and froze). Since this image cannot convert one, an H.265
pre-roll is played as-is, and an H.264 pre-roll on an H.265 encoder is **refused** — the hold
simply shows H.265 black for the wait, exactly as it does with no pre-roll at all. So to show
a pre-roll on an H.265 encoder, supply it already encoded as H.265. Left at its `h264`
default, nothing changes.

**Making an H.265 pre-roll.** What ah4c wants on an H.265 encoder is an **H.265 MPEG-TS
(`.ts`) video** at your channel's resolution (usually 1920x1080) — not a photo, and not a
HEIC. [ffmpeg](https://ffmpeg.org) (free, open-source, on every platform) makes one in a
single command. HandBrake cannot help here: it does not output `.ts`, and it will not
encode a still image at all.

From a **still image** — this loops the picture into a ten-second H.265 clip, scaled to
fill a 1080p frame:

```
ffmpeg -loop 1 -i photo.jpg -t 10 -r 30 \
  -vf "scale=1920:1080:force_original_aspect_ratio=increase,crop=1920:1080" \
  -c:v libx265 -pix_fmt yuv420p -an preroll.ts
```

From a **video clip** — re-encode it to H.265, keeping its length (change the scale to your
channel's resolution, or drop the `-vf` line if it is already the right size):

```
ffmpeg -i input.mp4 \
  -vf "scale=1920:1080:force_original_aspect_ratio=increase,crop=1920:1080" \
  -c:v libx265 -pix_fmt yuv420p -an preroll.ts
```

Then point `PREROLL_FILE` at the resulting `preroll.ts`, or drop it into the mounted
`preroll` directory, and ah4c copies it through unchanged. (Both commands drop audio with
`-an`, since the pre-roll plays silent; leave it off to keep the clip's own sound.)

Each hold is logged under `[HOLD]`: when it began, what it is showing, and when the encoder
took over along with how much filler was sent.

### Built-in ws-scrcpy for interacting directly with the streaming device:

<img width="1666" height="836" alt="screenshot-docker6-2026-08-23-11-17-28" src="https://github.com/user-attachments/assets/f65ee365-7d42-4db0-aa91-79a254eaefb9" />

<img width="1666" height="836" alt="screenshot-docker6-2026-08-23-11-18-21" src="https://github.com/user-attachments/assets/eb45beda-5c27-402b-bd23-e8913b362eaa" />

#### Docker Instructions

1. Download the Docker convenience script:
   $ curl -fsSL https://get.docker.com -o get-docker.sh
2. Install Docker:
   $ sudo sh get-docker.sh
3. Install Portainer:
   $ sudo docker run -d -p 8000:8000 -p 9000:9000 -p 9443:9443 --name portainer \
    --restart=always \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -v portainer_data:/data \
    cr.portainer.io/portainer/portainer-ce:latest
4. Configure Portainer and add androidhdmi-for-channels.yml via Portainer-Stacks:
   https://<hostname or IP of server>:9443
5. Add environment variable values to bottom section of Portainer-Stacks as defined in Docker compose.
6. Deploy container.
   Use re-pull image and redeploy slider if the container has been updated since the last time you downloaded it.
7. Check Portainer log for running container using Quick Actions button from Container list to check for errors.

#### Recommended Docker Compose for Portainer-Stacks:

```yaml
services:
  # 2026.08.28
  # GitHub home for this project with setup instructions: https://github.com/sullrich/ah4c
  # Docker Hub home for this project: https://hub.docker.com/repository/docker/bnhf/ah4c
  ah4c:
    image: bnhf/ah4c:${TAG:-latest}
    container_name: ${CONTAINER_NAME:-ah4c}
    hostname: ${HOSTNAME:-ah4c}
    dns_search: ${DOMAIN:-localdomain} # Specify the name of your LAN's domain, usually local or localdomain
    runtime: ${DOCKER_RUNTIME:-runc} # Closed captions only. Set DOCKER_RUNTIME=nvidia for an NVIDIA GPU with the CUDA engine build. Requires the NVIDIA container toolkit.
    devices:
      - ${GPU_DEVICE:-/dev/null} # Closed captions only. Set GPU_DEVICE=/dev/dri to let the Vulkan engine build use an Intel or AMD GPU. Left at the default it passes /dev/null, which always exists and does nothing.
    ports:
      - ${HOST_PORT:-7654}:7654 # Port used by this ah4c proxy
    environment:
      - IPADDRESS=${IPADDRESS} # Hostname or IP address of this ah4c extension to be used in M3U file (also add port number if not in M3U)
      - NUMBER_TUNERS=${NUMBER_TUNERS} # Number of tuners you'd like defined - add a matching TUNERn_IP and ENCODERn_URL line below for each beyond 9
      - TUNER1_IP=${TUNER1_IP} # Streaming device #1 with adb port in the form hostname:port or ip:port
      - TUNER2_IP=${TUNER2_IP} # Streaming device #2 with adb port in the form hostname:port or ip:port
      - TUNER3_IP=${TUNER3_IP} # Streaming device #3 with adb port in the form hostname:port or ip:port
      - TUNER4_IP=${TUNER4_IP} # Streaming device #4 with adb port in the form hostname:port or ip:port
      - TUNER5_IP=${TUNER5_IP} # Streaming device #5 with adb port in the form hostname:port or ip:port
      - TUNER6_IP=${TUNER6_IP} # Streaming device #6 with adb port in the form hostname:port or ip:port
      - TUNER7_IP=${TUNER7_IP} # Streaming device #7 with adb port in the form hostname:port or ip:port
      - TUNER8_IP=${TUNER8_IP} # Streaming device #8 with adb port in the form hostname:port or ip:port
      - TUNER9_IP=${TUNER9_IP} # Streaming device #9 with adb port in the form hostname:port or ip:port
      - ENCODER1_URL=${ENCODER1_URL} # Full URL for tuner #1 in the form http://hostname/stream or http://ip/stream
      - ENCODER2_URL=${ENCODER2_URL} # Full URL for tuner #2 in the form http://hostname/stream or http://ip/stream
      - ENCODER3_URL=${ENCODER3_URL} # Full URL for tuner #3 in the form http://hostname/stream or http://ip/stream
      - ENCODER4_URL=${ENCODER4_URL} # Full URL for tuner #4 in the form http://hostname/stream or http://ip/stream
      - ENCODER5_URL=${ENCODER5_URL} # Full URL for tuner #5 in the form http://hostname/stream or http://ip/stream
      - ENCODER6_URL=${ENCODER6_URL} # Full URL for tuner #6 in the form http://hostname/stream or http://ip/stream
      - ENCODER7_URL=${ENCODER7_URL} # Full URL for tuner #7 in the form http://hostname/stream or http://ip/stream
      - ENCODER8_URL=${ENCODER8_URL} # Full URL for tuner #8 in the form http://hostname/stream or http://ip/stream
      - ENCODER9_URL=${ENCODER9_URL} # Full URL for tuner #9 in the form http://hostname/stream or http://ip/stream
      - STREAMER_APP=${STREAMER_APP} # Streaming device name and streaming app you're using in the form scripts/streamer/app (use lowercase with slashes between as shown)
      - PYATV=${PYATV:-false} # Set to TRUE to run docker-start-pyatv.sh at container start for Apple TV tuners via pyatv, instead of the default docker-start.sh used for adb-based tuners. Case-insensitive; anything else runs the default.
      - CHANNELSIP=${CHANNELSIP} # Hostname or IP address of the Channels DVR server itself
      - ALERT_SMTP_SERVER=${ALERT_SMTP_SERVER} # The domainname:port of the SMTP server you'll be using like smtp.gmail.com:587. This is for sending ah4c alerts if tuning fails.
      - ALERT_AUTH_SERVER=${ALERT_AUTH_SERVER} # The auth server for the e-mail you'll be using like smtp.gmail.com
      - ALERT_EMAIL_FROM=${ALERT_EMAIL_FROM} # The e-mail address you'd like your ah4c failure alert e-mails to show as being from.
      - ALERT_EMAIL_PASS=${ALERT_EMAIL_PASS} # Gmail and Yahoo both support the creation of app-specific e-mail passwords, and this is the way to go! It's NOT recommended to use your everyday e-mail password.
      - ALERT_EMAIL_TO=${ALERT_EMAIL_TO} # The e-mail address you'd like your alert e-mails sent to.
      - ALERT_WEBHOOK_URL=${ALERT_WEBHOOK_URL} # URL to GET when an alert fires (same failures that trigger the e-mail); put $reason in the URL and it's replaced with the URL-encoded message. Blank disables it.
      - LIVETV_ATTEMPTS=${LIVETV_ATTEMPTS} # For FireTV Live Guide tuning only, set maximum number of attempts at finding the desired channel
      - CREATE_M3US=${CREATE_M3US:-false} # Set to true to create device-specific M3Us for use with Amazon Prime Premium channels -- requires a FireTV device
      - UPDATE_SCRIPTS=${UPDATE_SCRIPTS:-true} # Set to true if you'd like the sample scripts and STREAMER_APP scripts updated whether they exist or not
      - UPDATE_M3US=${UPDATE_M3US:-true} # Set to true if you'd like the sample m3us updated whether they exist or not
      - TZ=${TZ} # Your local timezone in Linux "tz" format
      - SPEED_MODE=${SPEED_MODE:-false} # Set to false if you'd like the target streaming app to be closed after each tuning cycle (limited script support).
      - KEEP_WATCHING=${KEEP_WATCHING} # In supported scripts, set the delay before resending a tuning deeplink to prevent "Are you still watching?" type messages. Examples: Use 4h for 4 hours or 240m for 240 minutes.
      - AUTOCROP_CHANNELS=${AUTOCROP_CHANNELS} # Space separated list of channels (by number) with black borders on 4 sides to autocrop while maintaining aspect ratio. Requires LinkPi Encoder!
      - LINKPI_HOSTNAME=${LINKPI_HOSTNAME} # Hostname or IP of the LinkPi Encoder's web API. Required for AUTOCROP_CHANNELS.
      - LINKPI_USERNAME=${LINKPI_USERNAME} # Username for the LinkPi Encoder's web API. Required for AUTOCROP_CHANNELS.
      - LINKPI_PASSWORD=${LINKPI_PASSWORD} # Password for the LinkPi Encoder's web API. Required for AUTOCROP_CHANNELS; not currently read by the bundled scripts, which log in with the LinkPi default password instead.
      - USER_SCRIPT=${USER_SCRIPT} # Path to a custom script to run alongside ah4c at container startup. Blank runs nothing extra.
      - NULL_FRAME_INSERTION=${NULL_FRAME_INSERTION:-false} # Set to TRUE to fill encoder stalls with MPEG-TS NULL packets (PID 0x1FFF) so the DVR never sees a zero-byte gap mid-recording. Case-insensitive (true/True/TRUE all work); anything else, including 1/yes, leaves the feature off.
      - PLAYBACK_DETECTION=${PLAYBACK_DETECTION:-false} # Set to TRUE to hold the stream until the device reports audio playing and the picture moving, then start on a keyframe, so recording begins on the program, not the loading screen. Requires adb; network tuners only. Case-insensitive; anything but true leaves it off.
      - PLAYBACK_STATIC_TIMEOUT=${PLAYBACK_STATIC_TIMEOUT} # Only used with PLAYBACK_DETECTION=TRUE. Seconds the box may keep its prior player/session before the check falls back to gating on motion alone. Default 2 suits Ospreys; apps like DirecTV that hold one player across channel changes need more. 0 or unset uses the default.
      - PLAYBACK_DELAY=${PLAYBACK_DELAY} # Hold every tune this long before handing the DVR the program, so a slow-starting app still records within the DVR's 30s window. Black or a mounted pre-roll fills the wait; the box's own video is never passed through. Accepts a bare number (seconds) or a duration like 30s/1m, capped at 10m. Network tuners only. Empty or 0 disables it.
      - ENCODER_CODEC=${ENCODER_CODEC:-h264} # The video codec your encoder outputs: h264 (default) or h265. Filler black/pre-roll must match it, or playback won't cross to the program at hand-off. An H.264 pre-roll is refused on an H.265 encoder (falls back to black). Leave at h264 unless your encoder is H.265. Case-insensitive; h265/hevc both work.
      - HEARTBEAT_INTERVAL=${HEARTBEAT_INTERVAL:-0} # In supported scripts (currently osprey), seconds between keepalive keyevents sent during playback to stop the app's UI inactivity timer from resetting the stream. Set to 0 to disable.
      - NVIDIA_VISIBLE_DEVICES=${NVIDIA_VISIBLE_DEVICES} # Closed captions only. Set to all alongside DOCKER_RUNTIME=nvidia to expose an NVIDIA GPU. Empty means no GPU and is the default.
      - NVIDIA_DRIVER_CAPABILITIES=${NVIDIA_DRIVER_CAPABILITIES} # Closed captions only. Set to compute,utility when using an NVIDIA GPU, so the driver the CUDA engine build needs is passed in.
    volumes:
      - ${HOST_DIR}/ah4c/scripts:/opt/scripts # pre/stop/bmitune.sh scripts will be stored in this bound host directory under streamer/app
      - ${HOST_DIR}/ah4c/m3u:/opt/m3u # m3u files will be stored here and hosted at http://<hostname or ip>:7654/m3u for use in Channels DVR - Custom Channels settings
      - ${HOST_DIR}/ah4c/adb:/root/.android # Persistent data directory for adb keys
      - ${HOST_DIR}/ah4c/captions:/opt/captions # Closed caption settings, and the speech model, engine and any GPU driver downloaded from the Closed Captions page. Stays empty unless you turn captions on
      - ${PREROLL_FILE:-${HOST_DIR}/ah4c/preroll}:/opt/preroll # A video or still image shown to the DVR instead of NULL packets, during a PLAYBACK_DELAY/PLAYBACK_DETECTION hold or a NULL_FRAME_INSERTION stall. Set PREROLL_FILE to a host path, or drop the file into this directory. Anything ffmpeg reads; prepared once at startup, loops until the real stream is ready
    restart: unless-stopped
```

adb-server and ws-scrcpy no longer need ports of their own: adb only talks to the tuners
from inside the container, and ws-scrcpy is reverse-proxied through the main port at
`/scrcpy`, so `HOST_PORT` (7654) is the only port to publish.

#### And, here's a sample of the environment variables that you'll need to provide:
```yaml
TAG=latest
CONTAINER_NAME=ah4c
HOSTNAME=ah4c
DOMAIN=localdomain tailxxxxx.ts.net
DOCKER_RUNTIME=runc
GPU_DEVICE=/dev/dri
HOST_PORT=7654
IPADDRESS=docker6:7654
NUMBER_TUNERS=5
TUNER1_IP=firestick-desk1:5555
ENCODER1_URL=http://linkpi-encoder2:8090/stream0
TUNER2_IP=firestick-desk2:5555
ENCODER2_URL=http://linkpi-encoder2:8090/stream1
TUNER3_IP=firestick-desk3:5555
ENCODER3_URL=http://linkpi-encoder2:8090/stream2
TUNER4_IP=firestick-desk4:5555
ENCODER4_URL=http://linkpi-encoder2:8090/stream3
TUNER5_IP=firestick-desk5:5555
ENCODER5_URL=http://linkpi-encoder2:8090/stream4
STREAMER_APP=scripts/firetv/dtvstreamdeeplinks
PYATV=false
CHANNELSIP=media-server10
ALERT_SMTP_SERVER=smtp.gmail.com:587
ALERT_AUTH_SERVER=smtp.gmail.com
ALERT_EMAIL_FROM=xxxxxxxxxx@gmail.com
ALERT_EMAIL_PASS=xxxxxxxxxxxxxxxx
ALERT_EMAIL_TO=xxxxxxxxxx@gmail.com
ALERT_WEBHOOK_URL=
LIVETV_ATTEMPTS=
CREATE_M3US=false
UPDATE_SCRIPTS=true
UPDATE_M3US=true
TZ=America/Denver
SPEED_MODE=false
KEEP_WATCHING=235m
AUTOCROP_CHANNELS=
LINKPI_HOSTNAME=
LINKPI_USERNAME=
LINKPI_PASSWORD=
USER_SCRIPT=
NULL_FRAME_INSERTION=false
PLAYBACK_DETECTION=true
PLAYBACK_STATIC_TIMEOUT=12
PLAYBACK_DELAY=
PREROLL_FILE=
ENCODER_CODEC=h264
HEARTBEAT_INTERVAL=
NVIDIA_VISIBLE_DEVICES=
NVIDIA_DRIVER_CAPABILITIES=
HOST_DIR=/data
```

Four of these are only used by closed captions — `GPU_DEVICE`, `DOCKER_RUNTIME`,
`NVIDIA_VISIBLE_DEVICES` and `NVIDIA_DRIVER_CAPABILITIES` — and leaving them at their
defaults (`GPU_DEVICE=/dev/null`, `DOCKER_RUNTIME=runc`, the two NVIDIA variables empty)
does nothing: captions run on the processor. `GPU_DEVICE` needs to point at something that
exists, which is why the default is `/dev/null` rather than empty. To use a GPU:

| Variable | Default | For an Intel or AMD GPU (Vulkan) | For an NVIDIA GPU (CUDA) |
| --- | --- | --- | --- |
| `GPU_DEVICE` | `/dev/null` | `/dev/dri` | leave at the default |
| `DOCKER_RUNTIME` | `runc` | leave at the default | `nvidia` |
| `NVIDIA_VISIBLE_DEVICES` | empty | leave empty | `all` |
| `NVIDIA_DRIVER_CAPABILITIES` | empty | leave empty | `compute,utility` |

The NVIDIA options need the NVIDIA container toolkit installed on the host. Nothing changes
in the image either way, and captions run on the processor if none of this is set.

#### Developer Instructions
First see https://github.com/sullrich/ah4c/blob/main/getting_started.txt

