### Eye candy

![image](static/status_ss.png)
(built in stats gui)

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

```yml
version: '3.9'
services:
  ah4c:
    image: bnhf/ah4c:testlatest
    container_name: ah4c
    hostname: ah4c
    dns_search: localdomain # Specify the name of your LAN's domain, usually local or localdomain
    ports:
      - 5037:5037 # Port used by adb-server
      - 7654:7654 # Port used by this androidhdmi-for-channels proxy
      - 7655:8000 # Port used by ws-scrcpy
    environment:
      - IPADDRESS=${IPADDRESS} # Hostname or IP address of this androidhdmi-for-channels extension to be used in M3U file (also add port number if not in M3U)
      - NUMBER_TUNERS=${NUMBER_TUNERS} # Number of tuners you'd like defined 1, 2, 3 or 4 supported
      - TUNER1_IP=${TUNER1_IP} # Streaming device #1 with adb port in the form hostname:port or ip:port
      - TUNER2_IP=${TUNER2_IP} # Streaming device #2 with adb port in the form hostname:port or ip:port
      - TUNER3_IP=${TUNER3_IP} # Streaming device #3 with adb port in the form hostname:port or ip:port
      - TUNER4_IP=${TUNER4_IP} # Streaming device #4 with adb port in the form hostname:port or ip:port
      - ENCODER1_URL=${ENCODER1_URL} # Full URL for tuner #1 in the form http://hostname/stream or http://ip/stream
      - ENCODER2_URL=${ENCODER2_URL} # Full URL for tuner #2 in the form http://hostname/stream or http://ip/stream
      - ENCODER3_URL=${ENCODER3_URL} # Full URL for tuner #3 in the form http://hostname/stream or http://ip/stream
      - ENCODER4_URL=${ENCODER4_URL} # Full URL for tuner #4 in the form http://hostname/stream or http://ip/stream
      - STREAMER_APP=${STREAMER_APP} # Streaming device name and streaming app you're using in the form scripts/streamer/app (use lowercase with slashes between as shown)
      - CHANNELSIP=${CHANNELSIP} # Hostname or IP address of the Channels DVR server itself
      #- ALERT_SMTP_SERVER="smtp.gmail.com:587"
      #- ALERT_AUTH_SERVER="smtp.gmail.com"
      #- ALERT_EMAIL_FROM=""
      #- ALERT_EMAIL_PASS=""
      #- ALERT_EMAIL_TO=""
      #- ALERT_WEBHOOK_URL=""
      - TZ=${TZ} # Your local timezone in Linux "tz" format
    volumes:
      - /data/ah4c/scripts:/opt/scripts # pre/stop/bmitune.sh scripts will be stored in this bound host directory under streamer/app
      - /data/ah4c/m3u:/opt/m3u # m3u files will be stored here and hosted at http://<hostname or ip>:7654/m3u for use in Channels DVR - Custom Channels settings
      - /data/ah4c/adb:/root/.android # Persistent data directory for adb keys
    restart: unless-stopped
```

#### And, here's a sample of the environment variables that you'll need to provide:

![screencapture-htpc6-9000-2023-08-16-16_33_04](https://github.com/bnhf/ah4c/assets/41088895/f7d5a59f-ca49-4c2b-a40a-617c3e0a516c)

#### Developer Instructions
First see https://github.com/sullrich/ah4c/blob/main/getting_started.txt

