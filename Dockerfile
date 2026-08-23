#docker buildx build --platform linux/amd64,linux/arm64 --build-arg CDVR_RELEASE=$(curl -s https://channels-dvr.s3.amazonaws.com/latest.txt | tr -d '\n') -f Dockerfile -t bnhf/ah4c:latest . --push --no-cache

# First Stage: Build ws-scrcpy and ah4c
FROM golang:bookworm AS builder

ARG TARGETARCH
ENV DEBIAN_FRONTEND=noninteractive

# Install dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    git nodejs npm python3 make g++ \
    && rm -rf /var/lib/apt/lists/*

# Build ws-scrcpy application
WORKDIR /ws-scrcpy
RUN git clone https://github.com/NetrisTV/ws-scrcpy.git . \
    && npm install && npm run dist

WORKDIR /ws-scrcpy/dist
RUN npm install

# Build ah4c application
WORKDIR /go/src/github.com/sullrich
RUN git clone --branch main --single-branch https://github.com/sullrich/ah4c . \
    && sh bump-version.sh \
    && go build -o /opt/ah4c

# Second Stage: Create the Runtime Environment
FROM debian:bookworm-slim AS runner
LABEL maintainer="The Slayer <slayer@technologydragonslayer.com>"

ARG TARGETARCH
ARG CDVR_RELEASE
ARG CDVR_URL=https://channels-dvr.s3.amazonaws.com
ENV DEBIAN_FRONTEND=noninteractive

# Add contrib/non-free/non-free-firmware components
RUN sed -i 's/^Components: .*/Components: main contrib non-free non-free-firmware/' /etc/apt/sources.list.d/debian.sources

# Install runtime dependencies (adb for Android-based tuners, nodejs/npm for ws-scrcpy, python3 for pyatv)
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl bash dnsutils procps nano tzdata jq bc \
    android-tools-adb tesseract-ocr \
    nodejs npm \
    python3 python3-pip \
    libva2 libva-drm2 vainfo \
    && rm -rf /var/lib/apt/lists/*

# Install pyatv from PyPI for PYATV=true (build-essential/python3-dev needed to compile miniaudio on arm64)
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential python3-dev \
    && pip3 install --break-system-packages pyatv \
    && pip3 show pyatv \
    && apt-get purge -y --auto-remove build-essential python3-dev \
    && rm -rf /var/lib/apt/lists/*

# Add Intel VA driver & (optionally) QSV libs only on amd64
RUN if [ "$TARGETARCH" = "amd64" ]; then \
      apt-get update && apt-get install -y --no-install-recommends \
        intel-media-va-driver-non-free libmfx1 && \
      rm -rf /var/lib/apt/lists/* ; \
    fi

# (Optional) set for Intel VA driver name
ENV LIBVA_DRIVER_NAME=iHD

# Download ffmpeg and ffprobe from Channels DVR
RUN ARCH=$([ "$TARGETARCH" = "amd64" ] && echo "x86_64" || echo "arm64") \
    && curl -fsSL ${CDVR_URL}/${CDVR_RELEASE}/ffmpeg-linux-${ARCH} -o /usr/bin/ffmpeg \
    && curl -fsSL ${CDVR_URL}/${CDVR_RELEASE}/ffprobe-linux-${ARCH} -o /usr/bin/ffprobe \
    && chmod +x /usr/bin/ffmpeg /usr/bin/ffprobe

# Set up working directories
RUN mkdir -p /opt/scripts /tmp/scripts /tmp/m3u /opt/html /opt/static

WORKDIR /opt

# Copy built files from builder
COPY --from=builder /ws-scrcpy/dist /opt/ws-scrcpy
COPY --from=builder /opt/ah4c /opt/ah4c

# Copy necessary scripts and static files
COPY docker-start.sh adbpackages.sh /opt/
COPY scripts /tmp/scripts/
COPY m3u/* /tmp/m3u/
COPY html/* /opt/html/
RUN sed -i '/href="\/config"/d; /href="\/env"/d' /opt/html/index.html
COPY static /opt/static/

# Ensure start script is executable
RUN chmod +x /opt/docker-start.sh \
    && groupadd render || true

# Expose needed ports
EXPOSE 7654 8000

# Run start script -- PYATV=true (case-insensitive) selects atvremote/pyatv tuners internally
CMD ["./docker-start.sh"]
