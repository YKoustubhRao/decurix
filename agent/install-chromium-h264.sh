#!/bin/sh
# Gives the desktop's Chromium an H.264 decoder. Run as root at image build.
#
# The base image ships Ubuntu's chromium-codecs-ffmpeg, the free build of
# Chromium's libffmpeg.so, which has no H.264 - yet Chromium still says
# H.264 is supported (MediaSource.isTypeSupported, canPlayType "probably",
# VideoDecoder.isConfigSupported all answer yes) and only fails when a
# decoder is made: "DECODER_ERROR_NOT_SUPPORTED: video decoder initialization
# failed". Measured in decurix-desktop:basic's Chromium 112. A phone app's
# picture is H.264 (scrcpy), and so is most video on the web.
#
# chromium-codecs-ffmpeg-extra is the same library with H.264, from the same
# Ubuntu source package. It has to be the exact version of chromium-browser
# (the library is internal to that build), so it is pinned with its hash and
# skipped - with a warning, not a failed build - if the base image ever
# moves to another Chromium.
set -eu

want=112.0.5615.49-0ubuntu0.18.04.1
sha256=9878ef8d11beb6845cb0e31c6be24f5cd64f832411387daab52a413c72e6b77b
url="http://archive.ubuntu.com/ubuntu/pool/universe/c/chromium-browser/chromium-codecs-ffmpeg-extra_${want}_amd64.deb"

have=$(dpkg-query -W -f='${Version}' chromium-browser 2>/dev/null || true)
if [ "$have" != "$want" ]; then
    echo "install-chromium-h264: Chromium is ${have:-not installed}, not $want; leaving its codecs alone (no H.264: phone apps will show no picture)" >&2
    exit 0
fi

if dpkg-query -W -f='${Status}' chromium-codecs-ffmpeg-extra 2>/dev/null | grep -q "install ok installed"; then
    exit 0
fi

deb=/tmp/chromium-codecs-ffmpeg-extra.deb
curl -fsSL -o "$deb" "$url"
echo "$sha256  $deb" | sha256sum -c -
# It Conflicts/Replaces chromium-codecs-ffmpeg, which dpkg removes for it;
# chromium-browser depends on either one.
dpkg -i "$deb"
rm -f "$deb"
