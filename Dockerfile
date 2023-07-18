# docker buildx build --platform linux/amd64,linux/arm64,linux/arm/v7 -f Dockerfile -t sullrich/androidhdmi-for-channels . --push --no-cache
FROM golang:bullseye AS builder
RUN apt update && apt install -y git
RUN mkdir -p /go/src/github.com/sullrich
WORKDIR /go/src/github.com/sullrich
RUN git clone https://github.com/sullrich/androidhdmi-for-channels .
RUN go build -o /opt/androidhdmi-for-channels

FROM debian:latest
RUN apt update && apt install -y adb curl iputils-ping
#RUN mkdir -p /opt/scripts /tmp/scripts /tmp/m3u /tmp/html
RUN mkdir -p /opt/scripts /tmp/scripts /tmp/m3u /opt/html /opt/static
#WORKDIR /opt/scripts
WORKDIR /opt
COPY --from=builder /opt/androidhdmi-for-channels* /opt
#COPY docker-start.sh ..
COPY docker-start.sh .
COPY scripts /tmp/scripts
COPY m3u/* /tmp/m3u
#COPY html/* /tmp/html
COPY html/* /opt/html
COPY static/* /opt/static
EXPOSE 7654
#CMD ../docker-start.sh
CMD ./docker-start.sh
