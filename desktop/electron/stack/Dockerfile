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
RUN apk add --no-cache docker-cli bash curl ca-certificates

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
