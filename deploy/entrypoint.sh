#!/bin/sh
# Fetch Google Chrome into the data volume the first time this container starts, then hand over
# to gpb.
#
# Chrome is not in the image on purpose. It is proprietary, and an image carrying it could not be
# published — which would put every reader back to building one themselves. Downloading it here
# leaves the published image free of Google code and makes the reader the one who accepts Chrome's
# terms, from Google's own server, exactly as they would installing it on a laptop.
#
# It lands in /data rather than in the container's filesystem so that a rebuilt image does not
# fetch it again. Chromium is not an option: it is signed out of Google within minutes, measured —
# see deploy/README.md.
set -eu

chrome_home="${GPB_CHROME_HOME:-/data/chrome}"
attempts=3

install_chrome() {
    deb="$chrome_home.deb"
    staging="$chrome_home.part"

    rm -rf "$staging" "$deb"
    curl --fail --silent --show-error --location --output "$deb" "$GPB_CHROME_DEB_URL"
    dpkg-deb -x "$deb" "$staging"
    rm -f "$deb"
    mv "$staging" "$chrome_home"
}

if [ ! -x "$GPB_CHROME_PATH" ]; then
    attempt=1
    while [ "$attempt" -le "$attempts" ]; do
        echo "gpb: fetching Google Chrome from $GPB_CHROME_DEB_URL (attempt $attempt of $attempts)"
        if install_chrome; then
            break
        fi
        echo "gpb: fetching Google Chrome failed"
        attempt=$((attempt + 1))
        sleep 5
    done
fi

if [ -x "$GPB_CHROME_PATH" ]; then
    echo "gpb: $("$GPB_CHROME_PATH" --version) at $GPB_CHROME_PATH"
else
    # Starting anyway is deliberate: without Chrome no backup can run, but the web UI is still
    # where the reader sets a password and reads what went wrong, and a container that refuses to
    # start says nothing to anybody. The next restart tries the download again.
    echo "gpb: WARNING — Google Chrome is not installed at $GPB_CHROME_PATH." \
         "Backups and sign-in will fail until it is; restart this container to try again."
fi

exec /usr/local/bin/gpb "$@"
