# Qualification overlay for an older basic image lacking Impress.
# Current desktop Dockerfiles already include it; this avoids rebuilding the
# entire desktop just to qualify the native presentation transfer cases.
FROM decurix-desktop:basic
USER root
RUN for try in 1 2 3; do apt-get -o Acquire::Retries=3 -o Acquire::http::Timeout=60 -o Acquire::https::Timeout=60 update && \
    apt-get -o Acquire::Retries=3 -o Acquire::http::Timeout=60 -o Acquire::https::Timeout=60 install -y --no-install-recommends libreoffice-impress && break; \
    [ "$try" = 3 ] && exit 1; echo "apt-get: try $try failed; trying again"; sleep $((try * 20 - 10)); done && \
    rm -rf /var/lib/apt/lists/* && \
    chmod 666 /etc/passwd /etc/group
USER headless
