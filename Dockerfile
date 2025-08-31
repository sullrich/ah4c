#docker buildx build --platform linux/amd64,linux/arm64 -f Dockerfile -t bnhf/ah4c:latest -t bnhf/ah4c:2025.08.31 . --push --no-cache

# First Stage: Build ws-scrcpy and ah4c
FROM golang:bookworm AS builder

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
RUN git clone https://github.com/sullrich/ah4c . \
    && go build -o /opt/ah4c

# Second Stage: Create the Runtime Environment
FROM debian:bookworm-slim AS runner
LABEL maintainer="The Slayer <slayer@technologydragonslayer.com>"

# Install runtime dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    android-tools-adb curl npm bash dnsutils ffmpeg procps nano tzdata tesseract-ocr \
    && rm -rf /var/lib/apt/lists/*

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
COPY static /opt/static/

# Expose needed ports
EXPOSE 7654
EXPOSE 8000

# Run start script
CMD ["./docker-start.sh"]
