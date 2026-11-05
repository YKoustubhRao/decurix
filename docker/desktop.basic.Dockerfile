# TheBot's soul binary, built from its real source (agent/thebot/) rather
# than a pre-committed binary - genuinely part of this repo and buildable
# by anyone's own CI, not an opaque blob. Pure-Go (modernc.org/sqlite, no
# CGO) and its whole UI is embedded via go:embed, so the build output is
# one self-contained static binary.
FROM golang:1.27 AS thebot-builder
# What this stage leaves behind is pruned after a build (desktop.PruneDanglingResources, resource plan P1 #10).
LABEL decurix.build-stage="1"
WORKDIR /src
COPY agent/thebot/go.mod agent/thebot/go.sum ./
# go has no download timeout of its own: ten minutes a try, three tries.
RUN for try in 1 2 3; do timeout 600 go mod download && exit 0; echo "go mod download: try $try failed or stalled"; sleep $((try * 20 - 10)); done; exit 1
COPY agent/thebot/cmd ./cmd
COPY agent/thebot/internal ./internal
COPY agent/thebot/web ./web
# Two cores and one package at a time: an unbounded build inside Docker took
# every core and a couple of GB at once and brought down the 8 GB WSL host
# this is developed on. It costs a minute of wall time, not correctness.
RUN GOMAXPROCS=2 CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -p 1 -trimpath -ldflags="-s -w" -o /soul ./cmd/soul

# The base is not pinned by digest yet: no digest is recorded anywhere here, and this loop could not pull to read one.
# The owner pins it (`docker image inspect --format '{{index .RepoDigests 0}}' accetto/ubuntu-vnc-xfce-chromium-g3`)
# and moves the pin on purpose (B-24).
FROM accetto/ubuntu-vnc-xfce-chromium-g3:latest

USER root

# Everything this image takes from Ubuntu, in one step: each apt-get update
# downloads the package indexes again (~25 MB), and five separate steps did
# that five times on every fresh build. What each group is for:
#
# - Computer use: what decurix_agent/computer needs to see and work the
#   screen. xdotool sends real input (XTEST); python3-xlib reads the
#   framebuffer and the window list; at-spi2-core, libatk-adaptor and the
#   Atspi bindings expose GTK, XFCE and LibreOffice widgets by role and name.
#   Chromium is read over its DevTools port instead (agent/chromium-browser.init),
#   which needs nothing installed. tesseract-ocr (+ English) reads the text of
#   a screen that is only pixels - a host app on a canvas - when a replay has
#   to decide whether a step is still needed or where a click lands now
#   (computer/ocr.py); without it those rungs are skipped, nothing breaks.
# - A worker's everyday tools: git, sqlite3, zip/unzip and 7z (archives people
#   send), ImageMagick (resize, convert, montage pictures), qpdf (split, merge,
#   decrypt PDFs), xclip and wmctrl (the clipboard and windows from a script).
#   Missing, each was a task worked around in Python or a person asked to help.
# - A modern look for the desktop a human actually opens: Ubuntu's own Yaru
#   theme (GTK + xfwm4, in a red accent to match Decurix's own palette),
#   Papirus icons and the Breeze cursor; Inter is a screen-tuned UI font, used
#   in place of the base image's plain "Sans" (applied by decurix-xsettings.xml
#   / decurix-xfwm4.xml below); whiskermenu is the taskbar's Start menu.
# - curl: the first thing a small model reaches for to read a page or an API;
#   without it a live request failed with "xargs: curl: No such file or
#   directory" and a person was asked to install it.
# - Back-office documents (decurix_agent/tools/office.py): poppler-utils reads
#   a PDF's text and turns a scanned page into a picture the hosted model can
#   read; openpyxl reads and writes workbooks; pypdf, img2pdf and PIL combine
#   scans and PDFs into one file; python-docx reads Word files.
# - LibreOffice Writer and Calc: where a person demonstrates spreadsheet and
#   document work, what the GUI benchmark's calc-*/writer-* tasks run in, and
#   what turns a Word or Excel file into a faithful PDF (tools/office.combine_pdf).
#   The developer/ai Dockerfiles install the full suite, but only this image is
#   built and tagged as all three.
# Every download bounded and the step tried again (internal/desktop/dockerfile_downloads_test.go): apt gives up on a
# connection silent for a minute and tries each file three times, and the whole download is tried three times, 10 s
# and then 30 s apart, so one bad moment of a CDN never fails the build.
RUN for try in 1 2 3; do apt-get -o Acquire::Retries=3 -o Acquire::http::Timeout=60 -o Acquire::https::Timeout=60 update && \
    apt-get -o Acquire::Retries=3 -o Acquire::http::Timeout=60 -o Acquire::https::Timeout=60 install -y --no-install-recommends \
        xdotool \
        tesseract-ocr \
        tesseract-ocr-eng \
        python3-xlib \
        python3-gi \
        gir1.2-atspi-2.0 \
        at-spi2-core \
        libatk-adaptor \
        python3-pip \
        python3-venv \
        yaru-theme-gtk \
        papirus-icon-theme \
        breeze-cursor-theme \
        fonts-inter \
        xfce4-whiskermenu-plugin \
        curl \
        ca-certificates \
        poppler-utils \
        python3-openpyxl \
        python3-pypdf \
        python3-docx \
        python3-pil \
        img2pdf \
        libreoffice-writer \
        libreoffice-calc \
        libreoffice-impress \
        libreoffice-gtk3 \
        gimp \
        libgail-common \
        inkscape \
        fonts-liberation \
        git \
        sqlite3 \
        zip \
        unzip \
        p7zip-full \
        imagemagick \
        qpdf \
        xclip \
        wmctrl && break; \
    [ "$try" = 3 ] && exit 1; echo "apt-get: try $try failed; trying again"; sleep $((try * 20 - 10)); done && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

# The base image sets NO_AT_BRIDGE=1, which stops every GTK app - the XFCE
# panel, Thunar, mousepad, LibreOffice's GTK front end - from publishing its
# widgets over AT-SPI at all. Without this the accessibility tree is empty.
ENV NO_AT_BRIDGE=0
# GTK2 apps (GIMP 2.10) publish their widgets only with these modules loaded (libgail-common, libatk-adaptor). GTK2 alone
# reads this variable, so GTK3 apps are untouched.
ENV GTK2_MODULES=gail:atk-bridge

# The browser: Google Chrome stable from Google's signed repository, in place of
# the base image's frozen Chromium 112, behind the one launcher every route to
# the browser ends in (agent/install-browser.sh, agent/chromium-browser). Its
# managed policies keep default-browser, sign-in and sync prompts - modal
# dialogs nobody on a desktop is there to answer - from ever appearing.
COPY agent/chromium-browser /tmp/chromium-browser
COPY agent/chrome-policies.json /etc/opt/chrome/policies/managed/decurix.json
COPY agent/install-browser.sh /tmp/install-browser.sh
RUN chmod 644 /etc/opt/chrome/policies/managed/decurix.json \
    && sh /tmp/install-browser.sh && rm -f /tmp/install-browser.sh /tmp/chromium-browser

# One Python for the agent and every worker script: the system's, /usr/bin/python3, which is the python3 on the PATH
# every desktop gets, real and practice alike (desktop.desktopPath). The libraries a worker's own scripts reach for
# are installed into it by its own pip, named by path. There was a venv for python-pptx first on the image's PATH: the
# "system" pip3 below then installed into the venv, practice desktops (which kept the image's PATH) had pandas and the
# rest, and real desktops (the control plane's PATH, no venv) had none of them (resource plan B1). One environment
# also drops the second copy of python-pptx and of numpy. Pinned, like everything here, but for requests and
# python-dateutil, which Ubuntu's own packages may already hold: those are floors (with a ceiling), so pip keeps an apt
# copy that satisfies them instead of replacing a package apt owns. --break-system-packages is this image's norm for
# worker installs (PIP_BREAK_SYSTEM_PACKAGES below). It must never shadow or replace an apt-owned package: pip writes
# what it installed (--report), and the build stops if any of it is a package apt installed too.
RUN for try in 1 2 3; do /usr/bin/python3 -m pip install --no-cache-dir --timeout 60 --retries 5 --break-system-packages \
        --report /tmp/pip-report.json \
        python-pptx==1.0.2 \
        pandas==2.2.3 \
        "requests>=2.31,<3" \
        beautifulsoup4==4.12.3 \
        pdfplumber==0.11.4 \
        pymupdf==1.24.14 \
        matplotlib==3.9.2 \
        "python-dateutil>=2.8.2,<3" \
        xlrd==2.0.1 \
        tabulate==0.9.0 \
        reportlab==4.2.5 && break; \
    [ "$try" = 3 ] && exit 1; echo "pip3: try $try failed; trying again"; sleep $((try * 20 - 10)); done \
    && /usr/bin/python3 -c "import json, re, sys, importlib.metadata as md; norm = lambda n: re.sub(r'[-_.]+', '-', n).lower(); apt = {norm(d.metadata['Name']) for d in md.distributions(path=['/usr/lib/python3/dist-packages']) if d.metadata['Name']}; got = {norm(i['metadata']['name']) for i in json.load(open('/tmp/pip-report.json'))['install']}; both = sorted(apt & got); print('pip installed over apt-owned packages: ' + ', '.join(both)) if both else None; sys.exit(1 if both else 0)" \
    && rm -f /tmp/pip-report.json

# Every worker library imports in the python3 a desktop runs, under exactly the PATH a desktop gets
# (desktop.desktopPath, pinned by internal/desktop/onepython_test.go): the build fails here, not a task later, if any
# is missing or broken.
RUN env -i PATH=/opt/tools/bin:/opt/tools/go/bin:/opt/tools/node/bin:/opt/tools/rust/bin:/home/headless/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin python3 -c "import pptx, pandas, requests, bs4, pdfplumber, fitz, matplotlib, dateutil, xlrd, tabulate, reportlab, openpyxl, docx, pypdf, PIL"

COPY agent/vm_server.py /opt/vm_server.py
COPY agent/decurix_agent /opt/decurix_agent
COPY agent/start_agent_terminal.sh /opt/start_agent_terminal.sh
# The `gui` command: the screen as a shell command, for the engines that
# work through shell commands (decurix_agent/computer/cli.py).
COPY agent/gui /usr/local/bin/gui
COPY agent/office /usr/local/bin/office
COPY agent/lookup /usr/local/bin/lookup
COPY agent/handoff /usr/local/bin/handoff
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
# end users should see Decurix branding, not generic XFCE art, the moment
# a desktop opens.
COPY agent/decurix-wallpaper.svg /opt/decurix-wallpaper.svg
# The taskbar's Start button icon (decurix-panel.xml's whiskermenu plugin) -
# an absolute path rather than an icon-theme name, so it renders regardless
# of which icon theme is active.
COPY agent/decurix-start-icon.svg /opt/decurix-start-icon.svg
# The browser's launch flags, without the base image's hardcoded --no-sandbox
# (see agent/chromium-browser.init). Seeded into /home/headless for new
# desktops, and kept in /opt for start_agent_terminal.sh to reinstall into
# existing ones, whose home is a volume this layer never reaches.
COPY agent/chromium-browser.init /opt/decurix-chromium.init
COPY agent/chromium-browser.init /home/headless/.chromium-browser.init
# The browser's desktop icon: the base image's "Chromium Web Browser" icon pointed at
# the removed package's icon image and offered Chromium-only actions. Same file name,
# so anything that looks it up still finds it; start_agent_terminal.sh swaps the old
# one on desktops created before.
COPY agent/decurix-desktop-browser.desktop /opt/decurix-desktop-browser.desktop
COPY agent/decurix-desktop-browser.desktop /home/headless/Desktop/chromium-browser.desktop
# A desktop launcher runs only when it is executable (xfdesktop asks "untrusted launcher?" otherwise).
RUN chmod 755 /home/headless/Desktop/chromium-browser.desktop

RUN chmod +x /opt/vm_server.py /opt/start_agent_terminal.sh /opt/soul /usr/local/bin/gui /usr/local/bin/office /usr/local/bin/lookup /usr/local/bin/handoff

RUN mkdir -p /home/headless/.config/autostart \
    /home/headless/.config/xfce4/xfconf/xfce-perchannel-xml \
    /home/headless/.config/xfce4/panel/launcher-3 \
    /home/headless/.config/xfce4/panel/launcher-4 \
    /home/headless/.config/xfce4/panel/launcher-5 \
    && chown -R headless:headless /home/headless/.config

COPY agent/decurix-agent.desktop /home/headless/.config/autostart/decurix-agent.desktop
# Pre-seeds four xfconf channels so XFCE starts already looking like
# Decurix instead of generating the base image's own defaults on first
# launch: the wallpaper (decurix-desktop-wallpaper.xml), the GTK
# theme/icons/font/cursor (decurix-xsettings.xml), the matching
# window-manager theme (decurix-xfwm4.xml) and a single bottom taskbar in
# place of the base image's own top-menu-bar-plus-autohide-dock layout
# (decurix-panel.xml) - a Start button (whiskermenu) with pinned launchers
# at the left, running windows centered, and the system tray/clock at the
# right, the layout a Windows user already knows. Each file only sets the
# properties it cares about - everything else still falls back to
# xfwm4/xfsettingsd's own defaults, verified live by diffing a fresh
# session's full property dump against one seeded with just these.
COPY agent/decurix-desktop-wallpaper.xml /home/headless/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-desktop.xml
COPY agent/decurix-xsettings.xml /home/headless/.config/xfce4/xfconf/xfce-perchannel-xml/xsettings.xml
COPY agent/decurix-xfwm4.xml /home/headless/.config/xfce4/xfconf/xfce-perchannel-xml/xfwm4.xml
COPY agent/decurix-panel.xml /home/headless/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-panel.xml
# The taskbar's three pinned launchers (Files/Browser/Terminal) - each
# xfce4-panel launcher plugin keeps its own desktop-entry under
# launcher-<plugin-id>/, keyed by the plugin ID hardcoded in
# decurix-panel.xml's own plugin-3/4/5, not by the desktop file's name.
COPY agent/decurix-launcher-files.desktop /home/headless/.config/xfce4/panel/launcher-3/decurix-files.desktop
COPY agent/decurix-launcher-browser.desktop /home/headless/.config/xfce4/panel/launcher-4/decurix-browser.desktop
COPY agent/decurix-launcher-terminal.desktop /home/headless/.config/xfce4/panel/launcher-5/decurix-terminal.desktop

RUN chmod 644 /home/headless/.config/autostart/decurix-agent.desktop \
    /home/headless/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-desktop.xml \
    /home/headless/.config/xfce4/xfconf/xfce-perchannel-xml/xsettings.xml \
    /home/headless/.config/xfce4/xfconf/xfce-perchannel-xml/xfwm4.xml \
    /home/headless/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-panel.xml \
    /home/headless/.config/xfce4/panel/launcher-3/decurix-files.desktop \
    /home/headless/.config/xfce4/panel/launcher-4/decurix-browser.desktop \
    /home/headless/.config/xfce4/panel/launcher-5/decurix-terminal.desktop \
    /home/headless/.chromium-browser.init /opt/decurix-chromium.init /opt/decurix-start-icon.svg \
    /opt/decurix-desktop-browser.desktop \
    && chown headless:headless \
    /home/headless/.config/autostart/decurix-agent.desktop \
    /home/headless/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-desktop.xml \
    /home/headless/.config/xfce4/xfconf/xfce-perchannel-xml/xsettings.xml \
    /home/headless/.config/xfce4/xfconf/xfce-perchannel-xml/xfwm4.xml \
    /home/headless/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-panel.xml \
    /home/headless/.config/xfce4/panel/launcher-3/decurix-files.desktop \
    /home/headless/.config/xfce4/panel/launcher-4/decurix-browser.desktop \
    /home/headless/.config/xfce4/panel/launcher-5/decurix-terminal.desktop \
    /home/headless/.chromium-browser.init /opt/decurix-start-icon.svg \
    /home/headless/Desktop/chromium-browser.desktop

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
# Shared tools runtime configuration:
# When decurix-tools is mounted at /opt/tools, environment scripts and path
# automatically expose all tools to interactive shells, desktop apps, and daemons.
RUN mkdir -p /opt/tools /var/cache/decurix /var/cache/decurix-models /etc/profile.d && \
    chmod 1777 /var/cache/decurix && \
    echo 'if [ -f /opt/tools/env.sh ]; then . /opt/tools/env.sh; fi' > /etc/profile.d/decurix-tools.sh && \
    chmod 644 /etc/profile.d/decurix-tools.sh

ENV PATH="/opt/tools/bin:/opt/tools/go/bin:/opt/tools/node/bin:/opt/tools/rust/bin:${PATH}" \
    GOROOT="/opt/tools/go" \
    RUSTUP_HOME="/opt/tools/rust" \
    CARGO_HOME="/home/headless/.cargo" \
    NPM_CONFIG_CACHE="/var/cache/decurix/npm" \
    GOPATH="/home/headless/go" \
    GOMODCACHE="/var/cache/decurix/go" \
    PIP_CACHE_DIR="/var/cache/decurix/pip" \
    HF_HOME="/var/cache/decurix/huggingface"

# The worker must not be able to become root, or nothing below root's reach (the approval gate's state, its token)
# is a boundary (docs/gate-boundary.md). The base image gave the desktop user `sudo` with the image's public default
# password, setuid helpers, and a world-writable /etc/passwd (so its startup could run `sudo chown` as the worker).
# All of that goes - every setuid and setgid bit in the image, not a list of the ones known today (B-21): a desktop
# runs with no-new-privileges, so none could work anyway, and Chromium uses its namespace sandbox (sandbox.go). The
# entrypoint does the one-time setup as root and then drops to the desktop user with no
# capabilities. The gate gets a user of its own.
RUN rm -f /etc/sudoers.d/headless /dockerstartup/.initial_sudo_password \
    && passwd -l root && passwd -l headless \
    && find / -xdev \( -perm -4000 -o -perm -2000 \) -type f -exec chmod ug-s {} + \
    && useradd --system --no-create-home --shell /usr/sbin/nologin decurixgate \
    && chmod 644 /etc/passwd /etc/group \
    && chown root:root /etc && chmod 755 /etc \
    && chown -R 1001:1001 /dockerstartup
# (/dockerstartup is handed to the desktop user here, at build time, and never by root at runtime: with hard-link
# protection off, a root `chown -R` of a folder the worker can write into hands over whatever it linked there.)
# /etc itself is root's, 755: the base image leaves it owned by a build user and group (984:980, 775). Nothing writes
# /etc at runtime but the entrypoint, as root (the gate key, the passwd/group modes); the desktop user's start scripts
# only test whether /etc/passwd is writable, which it is not either way.

# The X server answers only on its own socket, never on TCP: anything on the Docker network that reached X on port
# 6001 could drive this screen around the approval gate. Xvnc's own default is already no TCP; the mandatory config
# of TigerVNC's vncserver (which the base image's startup runs) makes it explicit, as `-nolisten tcp`. A desktop
# picks this up only when it is recreated. TestDesktopXServerHasNoTCPLive checks a running desktop.
# The VNC port (5901) stays on the desktop network: the control plane reads thumbnails over RFB from it there
# (desktop.captureFrame), and a desktop's only interfaces are loopback and that network, so binding it to one address
# would change nothing. What guards it is the per-desktop password (desktop.screenPassword); keeping desktops from
# reaching one another's ports at all is a network decision (docs/follow-ups.md, resource plan B7).
RUN mkdir -p /etc/tigervnc && echo 'nolisten=tcp' >> /etc/tigervnc/vncserver-config-mandatory
COPY agent/decurix-entrypoint.sh /opt/decurix-entrypoint.sh
# The agent compiled once, here, after its last COPY and before it is made root's (resource plan P1 #1): every `gui`
# call and every start of the agent and the gate imported it from source, about a second each, because the worker
# cannot write __pycache__ into a root-owned tree (and PYTHONDONTWRITEBYTECODE=1 says not to). The .pyc are root's,
# read-only, beside their sources. Python checks each against its source's mtime and size, so a tree copied over this
# one (a practice desktop's, a dev loop's docker cp) is compiled in memory as before, never run stale; the gate's
# `python3 -I` reads them too (isolated mode leaves bytecode caching alone).
RUN /usr/bin/python3 -m compileall -q -j 0 /opt/decurix_agent \
    && chmod 755 /opt/decurix-entrypoint.sh \
    && chown -R root:root /opt/decurix_agent /opt/decurix-entrypoint.sh \
    && chmod -R go-w /opt/decurix_agent

# A library a task needs beyond the ones installed above: `pip install --user NAME` puts it in the worker's own home
# (~/.local, on the desktop's persistent volume), so it is there after a restart and needs no administrator rights.
# Not PIP_USER=1: a venv's pip refuses a --user install, and the coding harness installs into project venvs.
ENV PIP_BREAK_SYSTEM_PACKAGES=1

# No USER line: the entrypoint starts as root for its setup and drops to headless (agent/decurix-entrypoint.sh).
ENTRYPOINT ["/usr/bin/tini", "--", "/opt/decurix-entrypoint.sh"]
