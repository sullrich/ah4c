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

ah4c WebUI:

<img width="1685" height="836" alt="screenshot-htpc6-2025-08-31-08-05-29" src="https://github.com/user-attachments/assets/ca64d967-29dd-4a78-97b5-1018d3ce2647" />

### Activity & logs:

![image](static/status_ss.png)
(built in stats gui)

### M3U Editor

<img width="1685" height="836" alt="screenshot-htpc6-2025-08-31-08-01-57" src="https://github.com/user-attachments/assets/f2297fc1-a108-4790-a78a-26401211beee" />

### Built-in ws-scrcpy for interacting directly with the streaming device:

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
  # 2024.10.30
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
      - NUMBER_TUNERS=${NUMBER_TUNERS} # Number of tuners you'd like defined 1, 2, 3, 4, 5, 6, 7, 8 or 9 supported
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
    volumes:
      - ${HOST_DIR}/ah4c/scripts:/opt/scripts # pre/stop/bmitune.sh scripts will be stored in this bound host directory under streamer/app
      - ${HOST_DIR}/ah4c/m3u:/opt/m3u # m3u files will be stored here and hosted at http://<hostname or ip>:7654/m3u for use in Channels DVR - Custom Channels settings
      - ${HOST_DIR}/ah4c/adb:/root/.android # Persistent data directory for adb keys
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
HOST_DIR=/data
```

#### Developer Instructions
First see https://github.com/sullrich/ah4c/blob/main/getting_started.txt

