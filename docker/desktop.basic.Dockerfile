# TheBot's soul binary, built from its real source (agent/thebot/) rather
# than a pre-committed binary - genuinely part of this repo and buildable
# by anyone's own CI, not an opaque blob. Pure-Go (modernc.org/sqlite, no
# CGO) and its whole UI is embedded via go:embed, so the build output is
# one self-contained static binary.
FROM golang:1.27 AS thebot-builder
WORKDIR /src
COPY agent/thebot/go.mod agent/thebot/go.sum ./
RUN go mod download
COPY agent/thebot/cmd ./cmd
COPY agent/thebot/internal ./internal
COPY agent/thebot/web ./web
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /soul ./cmd/soul

FROM accetto/ubuntu-vnc-xfce-chromium-g3:latest

USER root

COPY agent/vm_server.py /opt/vm_server.py
COPY agent/flowera_agent /opt/flowera_agent
COPY agent/start_agent_terminal.sh /opt/start_agent_terminal.sh
# TheBot harness's soul binary - built above from real source, started
# conditionally by start_agent_terminal.sh only when AGENT_HARNESS=thebot.
COPY --from=thebot-builder /soul /opt/soul
# docling_convert.py is only exercised for office-doc RAG ingestion
# (PDF/DOCX/etc - see internal/rag), which needs python3-pip + docling
# installed to actually work; this base image doesn't carry that, so
# those specific formats fail per-file, honestly, without breaking
# anything else - plain text/code files and every other skill (web
# search, terminal, file-write, memory) don't need Python at all.
COPY agent/thebot/scripts /opt/thebot-scripts
# Default desktop wallpaper, replacing the base image's stock XFCE one -
# end users should see Flowera branding, not generic XFCE art, the moment
# a desktop opens.
COPY agent/flowera-wallpaper.svg /opt/flowera-wallpaper.svg
# Chromium's launch flags, without the base image's hardcoded --no-sandbox
# (see agent/chromium-browser.init). Seeded into /home/headless for new
# desktops, and kept in /opt for start_agent_terminal.sh to reinstall into
# existing ones, whose home is a volume this layer never reaches.
COPY agent/chromium-browser.init /opt/flowera-chromium.init
COPY agent/chromium-browser.init /home/headless/.chromium-browser.init

RUN chmod +x /opt/vm_server.py /opt/start_agent_terminal.sh /opt/soul

RUN mkdir -p /home/headless/.config/autostart \
    /home/headless/.config/xfce4/xfconf/xfce-perchannel-xml \
    && chown -R headless:headless /home/headless/.config

COPY agent/flowera-agent.desktop /home/headless/.config/autostart/flowera-agent.desktop
# Pre-seeds xfce4-desktop's xfconf channel so XFCE starts on the Flowera
# wallpaper instead of generating its own default on first launch - see
# agent/flowera-desktop-wallpaper.xml for the actual property values.
COPY agent/flowera-desktop-wallpaper.xml /home/headless/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-desktop.xml

RUN chmod 644 /home/headless/.config/autostart/flowera-agent.desktop \
    /home/headless/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-desktop.xml \
    /home/headless/.chromium-browser.init /opt/flowera-chromium.init \
    && chown headless:headless \
    /home/headless/.config/autostart/flowera-agent.desktop \
    /home/headless/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-desktop.xml \
    /home/headless/.chromium-browser.init

# Restore the base image's own permissions on /etc/passwd and /etc/group.
#
# Real bug this fixes, found starting a fresh developer VM: the container
# exited immediately with
#   /dockerstartup/vnc_startup.rc: line 106: /dockerstartup/vnc.log: Permission denied
#
# The base image ships those files world-writable (666) on purpose. Its
# own startup runs user_generator.rc, whose ENTIRE body is gated on
# `if [[ -w /etc/passwd ]]` - and that body is what does
# `sudo chown -R $UID /dockerstartup $HOME`, which is the only reason the
# unprivileged headless user can write the VNC logs at all.
#
# Installing packages that create a system user (this image installs a
# lot of them) rewrites /etc/passwd with the system default 644/664. The
# generator then silently skips everything, /dockerstartup stays
# root-owned, vncserver cannot open its log, and the container dies with
# no VNC and no agent - looking like a broken image rather than a
# permission bit. Restoring 666 here, after every install and before the
# USER switch, puts the base image's mechanism back exactly as designed.
RUN chmod 666 /etc/passwd /etc/group

USER headless

ENTRYPOINT ["/usr/bin/tini", "--", "/dockerstartup/startup.sh"]
