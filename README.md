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

ah4c WebUI:

<img width="1685" height="836" alt="screenshot-htpc6-2025-08-31-08-05-29" src="https://github.com/user-attachments/assets/ca64d967-29dd-4a78-97b5-1018d3ce2647" />

### Activity & logs:

<img width="1685" height="836" alt="screenshot-channels0-2025-08-31-09-13-54" src="https://github.com/user-attachments/assets/8a3043cc-063a-401c-b49b-68c94ce74195" />

(built in stats gui)

### M3U Editor

<img width="1685" height="836" alt="screenshot-htpc6-2025-08-31-08-01-57" src="https://github.com/user-attachments/assets/f2297fc1-a108-4790-a78a-26401211beee" />

### Closed Captions

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

Audio is pulled out of the encoder's transport stream, transcribed on the CPU by an
NVIDIA Parakeet model, and written back into the video as CEA-608 caption data carried
in ATSC A/53 user data — the same carriage an HDHomeRun uses for over-the-air captions.
Channels DVR, VLC and anything else see a real closed caption track and offer it under
the usual subtitles button.

- **Not burned in, and not re-encoded.** The compressed video is passed through
  untouched; only a small caption message is inserted ahead of each picture, and the
  picture data comes out byte-for-byte identical. Quality, bitrate and tune time are
  unchanged, and streams that already carry captions are left alone.
- **Nothing is added to the Docker image.** No new packages, no Python, no model.
- **ah4c stays pure Go.** Recognition runs against
  [parakeet.cpp](https://github.com/mudler/parakeet.cpp) or
  [transcribe.cpp](https://github.com/handy-computer/transcribe.cpp), ggml engines opened
  at run time with purego. There is no cgo and nothing linked into the binary —
  `CGO_ENABLED=0` still builds.
- **Nothing is gated on an environment variable.** Everything is controlled from the web
  UI and stored in `captions/config.json`. Changes apply to the next tune.
- **No GPU, no /dev/dri, no hardware encoder.** It runs several times faster than real
  time on an ordinary CPU.
- **Entirely opt-in.** With captions off, a tune takes exactly the path it always did.

The page offers a small engine plus your choice of model. Nothing is bundled and
nothing is fetched until you ask for it. Every download URL is shown on the page next to
the button, so it is always clear what is being fetched and from where.

**Speech engine** (required) — [parakeet.cpp](https://github.com/mudler/parakeet.cpp) built for
your platform, from that project's GitHub releases. Four builds exist and the page offers
whichever this container can actually load:

| Build | Size | Needs |
| --- | --- | --- |
| CPU | ~1 MB | Nothing. Fast enough for several tuners at once |
| GPU via Vulkan | 59 MB | A Vulkan driver in the container and `/dev/dri` passed through |
| GPU via CUDA | 537 MB | The NVIDIA container runtime; the download carries its own CUDA runtime |
| GPU via CUDA 12 | 722 MB | The same, for older drivers |

None of them change the image, and the page tells you at a glance whether acceleration is
actually working or which piece is missing.

**Vulkan** covers Intel and AMD graphics. The driver is not in the image, so the page
downloads it the same way it downloads a model: the packages and everything they depend on
are saved into `captions/drivers` inside the bind mount, and put back automatically
at startup after a rebuild, without needing the network again.

You also have to pass the graphics device through, which is off by default. Set this in your
env file and recreate the container:

```
GPU_DEVICE=/dev/dri:/dev/dri
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

A GPU build is greyed out until the library it needs is loadable, which is settled by asking
the dynamic loader rather than by guessing. If a GPU build is selected but cannot run, the
engine falls back to the processor rather than failing, so captions keep working. Apple
silicon gets Metal in its single build and has no choice to make; arm64 Linux has no CUDA
build upstream and is offered CPU and Vulkan.

Quick Sync is not on that list and cannot be. It is fixed-function video encode and decode
hardware, not a compute unit, so nothing can run a model on it. The VA-API packages already
in the image are for video and are unrelated.

**Models**, from Hugging Face. Each names the engine that runs it, and the page downloads
that engine when you pick the model:

| Model | Accuracy | Delay | Size | Languages | Engine |
| --- | --- | --- | --- | --- | --- |
| **Nemotron 3.5 Streaming 0.6B** *(default)* — continuous, punctuated, sentence case | Good — 3.0% | Under a second | 938 MB | 25 | parakeet.cpp |
| **Parakeet Unified 0.6B** — continuous and punctuated, twice as accurate on English | Excellent — 1.4% | About two seconds | 731 MB | English | transcribe.cpp |
| **Multitalker Parakeet Streaming 0.6B** — trained on people talking over each other | Very good — 2.2% | About a second | 734 MB | English | transcribe.cpp |
| **Parakeet Realtime 120M** — just as quick, no punctuation | Basic | Under a second | 168 MB | English | parakeet.cpp |
| **Cohere Transcribe 03-2026** — the most accurate open model there is | Best — 1.3% | 3–4 seconds | 2.4 GB | 8 | transcribe.cpp |
| **Parakeet TDT 0.6B v3** — waits for a phrase, multilingual | Very good | 3–4 seconds | 897 MB | 25 | parakeet.cpp |
| **Parakeet TDT-CTC 110M** — phrase at a time, English | Good | 3–4 seconds | 170 MB | English | parakeet.cpp |

Accuracy is word error rate on LibriSpeech test-clean, the one benchmark all of these
publish. Read it as a ranking rather than a promise: it is clean read speech, and live
television is harder than that for every model in the list. The two entries without a
figure have no published number on it, and carry a rating rather than a guess.

The streaming models transcribe as the audio arrives rather than waiting for a phrase to
finish, which is the difference between captions about a second behind and captions three
or four seconds behind.

Punctuation is the other axis, and the reason the Nemotron model is the default: it is
continuous, punctuated, multilingual and quick, which no other single entry manages. The
120M produces no punctuation at all — NVIDIA's model card is explicit that it outputs
neither punctuation nor capitalisation, and no setting changes that — so it is there for
hardware that cannot spare the cores.

**Cohere Transcribe wants a GPU.** It is the most accurate model here and the top of the
public open-ASR leaderboard, but it is a 2B model that decodes a word at a time and cannot
transcribe continuously, so it reads a whole phrase before writing it. On a GPU that is
comfortable. On a fast desktop CPU it only just keeps pace with live audio, and on a NAS it
will fall behind and drop speech. Give it CUDA or Vulkan and it is the best captioning in
the list; leave it on a modest processor and one of the streaming models will serve you
better. It is offered in eight languages rather than the fourteen it knows, because CEA-608
cannot carry Japanese, Chinese, Korean, Arabic or Greek — those would come out blank rather
than wrong.

Captions are rendered in capitals, which is the long-standing convention for broadcast
captioning and is easier to read across a room; there is a setting for mixed case. Subtitle
files keep the natural sentence case regardless, since a file is read close up.

The right build for the machine is chosen automatically, so the arm64 image fetches the
arm64 engine without being told.

Both engine and model land in `/opt/captions`, which is why that volume has to exist. On
the host they sit in `${HOST_DIR}/ah4c/captions`, beside the `scripts`, `m3u` and `adb`
directories ah4c already keeps there. Remove either download from the page to reclaim the
space.

Captions appear a second or two after the words are spoken, because a phrase has to
finish before it can be recognized — the same lag live broadcast captioning has. An
optional extra delay is available if you want to push them back further.


### Built-in ws-scrcpy for interacting directly with the streaming device:

<img width="1685" height="836" alt="screenshot-htpc6-2025-08-31-08-17-02" src="https://github.com/user-attachments/assets/b84e6fb9-3e56-41c1-bb54-76bc70e69b27" />

<img width="1685" height="836" alt="screenshot-htpc6-2025-08-31-08-10-42" src="https://github.com/user-attachments/assets/a7e4ab65-1787-490f-a2a3-f02c6a2cc819" />

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
  # 2026.08.05
  # GitHub home for this project with setup instructions: https://github.com/sullrich/ah4c
  # Docker Hub home for this project: https://hub.docker.com/repository/docker/bnhf/ah4c
  ah4c:
    image: bnhf/ah4c:${TAG}
    container_name: ah4c
    hostname: ah4c
    dns_search: ${DOMAIN} # Specify the name of your LAN's domain, usually local or localdomain
    ports:
      - ${ADBS_PORT}:5037 # Port used by adb-server
      - ${HOST_PORT}:7654 # Port used by this ah4c proxy
      - ${WSCR_PORT}:8000 # Port used by ws-scrcpy
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
      - CHANNELSIP=${CHANNELSIP} # Hostname or IP address of the Channels DVR server itself
      - ALERT_SMTP_SERVER=${ALERT_SMTP_SERVER} # The domainname:port of the SMTP server you'll be using like smtp.gmail.com:587. This is for sending ah4c alerts if tuning fails.
      - ALERT_AUTH_SERVER=${ALERT_AUTH_SERVER} # The auth server for the e-mail you'll be using like smtp.gmail.com
      - ALERT_EMAIL_FROM=${ALERT_EMAIL_FROM} # The e-mail address you'd like your ah4c failure alert e-mails to show as being from.
      - ALERT_EMAIL_PASS=${ALERT_EMAIL_PASS} # Gmail and Yahoo both support the creation of app-specific e-mail passwords, and this is the way to go! It's NOT recommended to use your everyday e-mail password.
      - ALERT_EMAIL_TO=${ALERT_EMAIL_TO} # The e-mail address you'd like your alert e-mails sent to.
      #- ALERT_WEBHOOK_URL=""
      - LIVETV_ATTEMPTS=${LIVETV_ATTEMPTS} # For FireTV Live Guide tuning only, set maximum number of attempts at finding the desired channel
      - CREATE_M3US=${CREATE_M3US} # Set to true to create device-specific M3Us for use with Amazon Prime Premium channels -- requires a FireTV device
      - UPDATE_SCRIPTS=${UPDATE_SCRIPTS} # Set to true if you'd like the sample scripts and STREAMER_APP scripts updated whether they exist or not
      - UPDATE_M3US=${UPDATE_M3US} # Set to true if you'd like the sample m3us updated whether they exist or not
      - TZ=${TZ} # Your local timezone in Linux "tz" format
      - SPEED_MODE=${SPEED_MODE} # Set to false if you'd like the target streaming app to be closed after each tuning cycle (limited script support).
      - KEEP_WATCHING=${KEEP_WATCHING} # In supported scripts, set the delay before resending a tuning deeplink to prevent "Are you still watching?" type messages. Examples: Use 4h for 4 hours or 240m for 240 minutes.
      - NULL_FRAME_INSERTION=${NULL_FRAME_INSERTION} # Set to TRUE to fill encoder stalls with MPEG-TS NULL packets (PID 0x1FFF) so the DVR never sees a zero-byte gap mid-recording. Case-insensitive (true/True/TRUE all work); anything else, including 1/yes, leaves the feature off.
      - PLAYBACK_DETECTION=${PLAYBACK_DETECTION} # Set to TRUE to hold the stream until the device reports media audio playing and the picture is actually moving, then start on a keyframe, so a recording begins on the program rather than on the app's loading screen. Requires adb access to the tuner; network tuners only. Case-insensitive (true/True/TRUE all work); anything else, including 1/yes, leaves the feature off.
      - PLAYBACK_DELAY=${PLAYBACK_DELAY} # Set to a whole number of seconds to skip the start of each tune, so a recording begins on the program rather than on the app's loading screen. Piped through the bundled ffmpeg with -ss and stream copy; no re-encoding, and the skip starts on the next keyframe so it can run slightly past the configured value. The value is the total tune time, scripts included. Supported range is 2 to 30, since the DVR allows a tune about 30 seconds; values outside the range are clamped and logged. Ignored when PLAYBACK_DETECTION is TRUE; network tuners only. 0 or unset leaves the feature off.
      - HEARTBEAT_INTERVAL=${HEARTBEAT_INTERVAL} # In supported scripts (currently osprey), seconds between keepalive keyevents sent during playback to stop the app's UI inactivity timer from resetting the stream. Set to 0 to disable.
      - NVIDIA_VISIBLE_DEVICES=${NVIDIA_VISIBLE_DEVICES} # Closed captions only. Set to all alongside DOCKER_RUNTIME=nvidia to expose an NVIDIA GPU. Empty means no GPU and is the default
      - NVIDIA_DRIVER_CAPABILITIES=${NVIDIA_DRIVER_CAPABILITIES} # Closed captions only. Set to compute,utility when using an NVIDIA GPU, so the driver the CUDA engine build needs is passed in
    volumes:
      - ${HOST_DIR}/ah4c/scripts:/opt/scripts # pre/stop/bmitune.sh scripts will be stored in this bound host directory under streamer/app
      - ${HOST_DIR}/ah4c/m3u:/opt/m3u # m3u files will be stored here and hosted at http://<hostname or ip>:7654/m3u for use in Channels DVR - Custom Channels settings
      - ${HOST_DIR}/ah4c/adb:/root/.android # Persistent data directory for adb keys
      - ${HOST_DIR}/ah4c/captions:/opt/captions # Closed caption settings, and the speech model, engine and any GPU driver downloaded from the Closed Captions page. Stays empty unless you turn captions on
    devices:
      - ${GPU_DEVICE} # Closed captions only. Set GPU_DEVICE=/dev/dri:/dev/dri to let the Vulkan engine build use an Intel or AMD GPU. Left at the default it passes /dev/null, which always exists and does nothing
    runtime: ${DOCKER_RUNTIME} # Closed captions only. Set DOCKER_RUNTIME=nvidia for an NVIDIA GPU with the CUDA engine build. Requires the NVIDIA container toolkit
    restart: unless-stopped
```

#### And, here's a sample of the environment variables that you'll need to provide:
```yaml
TAG=latest
DOMAIN=localdomain tailxxxxx.ts.net
ADBS_PORT=5037
HOST_PORT=7654
SCRC_PORT=7655
IPADDRESS=htpc6:7654
NUMBER_TUNERS=5
TUNER1_IP=firestick-rack1:5555
ENCODER1_URL=http://encoder_48007/0.ts
TUNER2_IP=firestick-rack2:5555
ENCODER2_URL=http://encoder_48007/4.ts
TUNER3_IP=firestick-rack3:5555
ENCODER3_URL=http://encoder_48007/8.ts
TUNER4_IP=firestick-rack4:5555
ENCODER4_URL=http://encoder_48007/12.ts
TUNER5_IP=firestick-travel2:5555
ENCODER5_URL=http://encoder_23393/0.ts
STREAMER_APP=scripts/firetv/dtvdeeplinks
CHANNELSIP=media-server6
ALERT_SMTP_SERVER=smtp.gmail.com:587
ALERT_AUTH_SERVER=smtp.gmail.com
ALERT_EMAIL_FROM=xxxxxxxxxx@gmail.com
ALERT_EMAIL_PASS=xxxxxxxxxxxxxxxx
ALERT_EMAIL_TO=xxxxxxxxxx@gmail.com
UPDATE_SCRIPTS=true
UPDATE_M3US=true
TZ=US/Mountain
SPEED_MODE=false
KEEP_WATCHING=4h
NULL_FRAME_INSERTION=FALSE
PLAYBACK_DETECTION=FALSE
PLAYBACK_DELAY=0
HEARTBEAT_INTERVAL=0
HOST_DIR=/data
GPU_DEVICE=/dev/null:/dev/null
DOCKER_RUNTIME=runc
NVIDIA_VISIBLE_DEVICES=
NVIDIA_DRIVER_CAPABILITIES=
```

The last four are only used by closed captions, and the values above are the defaults that
do nothing: leave them exactly as they are unless you want a GPU build of the speech engine.
`GPU_DEVICE` passes `/dev/null` rather than being empty because a device mapping has to
point at something that exists. To use a GPU:

| Variable | Default | For an Intel or AMD GPU (Vulkan) | For an NVIDIA GPU (CUDA) |
| --- | --- | --- | --- |
| `GPU_DEVICE` | `/dev/null:/dev/null` | `/dev/dri:/dev/dri` | leave at the default |
| `DOCKER_RUNTIME` | `runc` | leave at the default | `nvidia` |
| `NVIDIA_VISIBLE_DEVICES` | empty | leave empty | `all` |
| `NVIDIA_DRIVER_CAPABILITIES` | empty | leave empty | `compute,utility` |

The NVIDIA options need the NVIDIA container toolkit installed on the host. Nothing changes
in the image either way, and captions run on the processor if none of this is set.

#### Developer Instructions
First see https://github.com/sullrich/ah4c/blob/main/getting_started.txt

