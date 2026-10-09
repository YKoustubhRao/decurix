#!/bin/sh
# The desktop's web browser: Google Chrome stable, installed from Google's
# signed apt repository, behind Decurix's one launcher (agent/chromium-browser).
# Run as root at image build.
#
# Why not the base image's Chromium: it is Ubuntu 18.04's last deb (112, 2023),
# frozen there - Ubuntu 24.04 ships Chromium only as a snap, which cannot run in
# a container. 112 lacks what current web apps assume (Promise.withResolvers,
# Object.groupBy), causing modern web clients to load a blank page. Chrome stable
# supports these APIs, carries H.264 (a phone app's picture;
# the old image needed a codec swap pinned to 112), and is updated by rebuilding
# the image.
#
# Every way of starting the browser ends in the one launcher - the panel's Web
# Browser button (x-www-browser), xdg-open, a launcher in $HOME, the agent's
# `open`, `google-chrome` typed in a terminal - so it always gets the profile,
# the DevTools port the agent reads pages through, and ~/.chromium-browser.init.
set -eu

# Google's Linux signing key (primary). Subkeys rotate - the newest is fetched
# with the key - but the primary never changes; anything else is refused.
KEY_FINGERPRINT=EB4C1BFD4F042F6DDDCCEC917721F63BD38B4796

export DEBIAN_FRONTEND=noninteractive

# 1. The old Chromium, held by the base image.
apt-mark unhold chromium-browser chromium-browser-l10n chromium-codecs-ffmpeg chromium-codecs-ffmpeg-extra >/dev/null 2>&1 || true
apt-get purge -y chromium-browser chromium-browser-l10n chromium-codecs-ffmpeg chromium-codecs-ffmpeg-extra >/dev/null 2>&1 || true
rm -rf /usr/lib/chromium-browser /etc/chromium-browser

# The launcher takes the old package's command name (launchers already written
# into desktops' home folders call it). Installed after the purge, which
# removes whatever file the old package owned at that path.
install -m 755 /tmp/chromium-browser /usr/bin/chromium-browser

# 2. Google's repository, pinned to the verified key. /etc/default/google-chrome
#    stops the package's postinst and daily cron job from rewriting the source
#    list without signed-by, which apt then rejects as a conflicting duplicate.
curl -fsSL https://dl.google.com/linux/linux_signing_key.pub -o /tmp/google-linux.pub
got=$(gpg --show-keys --with-colons /tmp/google-linux.pub 2>/dev/null | awk -F: '/^fpr/ {print $10; exit}')
if [ "$got" != "$KEY_FINGERPRINT" ]; then
    echo "install-browser: Google's signing key has fingerprint '$got', expected $KEY_FINGERPRINT - refusing to install" >&2
    exit 1
fi
gpg --dearmor < /tmp/google-linux.pub > /usr/share/keyrings/google-chrome.gpg
rm -f /tmp/google-linux.pub

printf 'repo_add_once="false"\nrepo_reenable_on_distupgrade="false"\n' > /etc/default/google-chrome
echo "deb [arch=amd64 signed-by=/usr/share/keyrings/google-chrome.gpg] https://dl.google.com/linux/chrome/deb/ stable main" \
    > /etc/apt/sources.list.d/google-chrome.list

apt-get update
apt-get install -y --no-install-recommends google-chrome-stable

# 3. Chrome's own command leads to the launcher. google-chrome-stable is the
#    package's file (dpkg-divert, so an upgrade puts its copy beside ours, not
#    over it); google-chrome is an alternative that points at it.
dpkg-divert --quiet --local --rename --add /usr/bin/google-chrome-stable
ln -sf /usr/bin/chromium-browser /usr/bin/google-chrome-stable

# 4. The system's browser is the launcher (the panel's Web Browser button goes
#    through x-www-browser), and web links open in it.
update-alternatives --install /usr/bin/x-www-browser x-www-browser /usr/bin/chromium-browser 300
update-alternatives --set x-www-browser /usr/bin/chromium-browser
update-alternatives --install /usr/bin/gnome-www-browser gnome-www-browser /usr/bin/chromium-browser 300
update-alternatives --set gnome-www-browser /usr/bin/chromium-browser
mkdir -p /etc/xdg
cat > /etc/xdg/mimeapps.list <<'EOF'
[Default Applications]
x-scheme-handler/http=google-chrome.desktop
x-scheme-handler/https=google-chrome.desktop
text/html=google-chrome.desktop
application/xhtml+xml=google-chrome.desktop
EOF

apt-get clean
rm -rf /var/lib/apt/lists/*

version=$(/opt/google/chrome/google-chrome --version 2>/dev/null || true)
echo "install-browser: ${version:-Google Chrome} installed behind /usr/bin/chromium-browser"
