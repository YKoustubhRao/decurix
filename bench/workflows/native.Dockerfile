# Qualification overlay for an older basic image lacking Impress.
# Current desktop Dockerfiles already include it; this avoids rebuilding the
# entire desktop just to qualify the native presentation transfer cases.
FROM decurix-desktop:basic
USER root
RUN apt-get update && \
    apt-get install -y --no-install-recommends libreoffice-impress && \
    rm -rf /var/lib/apt/lists/* && \
    chmod 666 /etc/passwd /etc/group
USER headless
