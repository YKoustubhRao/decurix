# The control plane's image, as the app builds it on the person's computer
# (desktop/electron/main.js, startStack). It is the runtime stage of
# docker/Dockerfile with the binary already built: scripts/*/build_app.* compile
# it and ship it in the app beside this file, so the computer needs nothing
# else - no Go, no checkout. Keep the two in step.
FROM alpine:3.24

WORKDIR /app

# A static binary that runs only docker (the host-app hop) and bash in here:
# Alpine's docker-cli is a few MB from apk, where Debian's docker.io took about
# five minutes of a first start.
# Bounded, as every download in a Dockerfile here is (internal/desktop/dockerfile_downloads_test.go), and never
# tied to one mirror: dl-cdn's community index opened and then sent nothing on the owner's network, and an apk
# here held a first start for 18 minutes. Three minutes a mirror (busybox timeout), each of Alpine's official
# mirrors in turn: the list desktop/electron/wsl/apk-safe.sh tries.
RUN for mirror in https://dl-cdn.alpinelinux.org/alpine https://mirrors.edge.kernel.org/alpine https://uk.alpinelinux.org/alpine; do \
        sed -i -E "s#^[a-z]+://[^ ]*(/(v[0-9]+\.[0-9]+|edge)/)#${mirror}\1#" /etc/apk/repositories; \
        timeout 180 apk add --no-cache docker-cli bash curl ca-certificates && exit 0; \
        echo "apk add: failed or did not finish within 180 s from ${mirror}; trying the next mirror"; \
    done; echo "apk add: failed from every Alpine mirror" >&2; exit 1

# decurix-android-arm64 is the phone build /phone/decurix hands out.
COPY decurix decurix-android-arm64 ./
# A file copied out of an app bundle keeps whatever mode it was given there.
RUN chmod 755 decurix

# The person's settings (the .env the app copies beside this file at each start) are never copied into an image
# layer, where anyone with the image could read the keys in it: compose hands them to the container when it starts
# (env_file in the staged docker-compose.yaml).

ENV DECURIX_DATA_DIR=/data
VOLUME /data

EXPOSE 8080

CMD ["./decurix"]
