# The control plane's image, as the Mac and Linux apps build it on the person's
# computer (desktop/electron/main.js, startStack). It is the runtime stage of
# docker/Dockerfile with the binary already built: scripts/*/build_app.sh
# compile it and ship it in the app beside this file, so the computer needs
# Docker and nothing else - no Go, no checkout. Keep the two in step.
FROM debian:bookworm-slim

WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends \
    docker.io \
    ca-certificates \
    curl \
    && rm -rf /var/lib/apt/lists/*

# decurix-android-arm64 is the phone build /phone/decurix hands out.
COPY decurix decurix-android-arm64 ./
# A file copied out of an app bundle keeps whatever mode it was given there.
RUN chmod 755 decurix

# The person's settings (the app copies their .env here at each start).
COPY .env ./

ENV DECURIX_DATA_DIR=/data
VOLUME /data

EXPOSE 8080

CMD ["./decurix"]
