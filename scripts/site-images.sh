#!/bin/sh
# Regenerate the site's share image and icons from site/src (needs Google Chrome; macOS sips).
set -eu
cd "$(dirname "$0")/../site/src"
CH="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
P=../public
"$CH" --headless=new --disable-gpu --hide-scrollbars --force-device-scale-factor=1 --window-size=1200,630 --screenshot=$P/og.png "file://$PWD/og.html"
"$CH" --headless=new --disable-gpu --hide-scrollbars --force-device-scale-factor=1 --default-background-color=00000000 --window-size=512,512 --screenshot=$P/icon-512.png "file://$PWD/icon.html"
sips -z 180 180 $P/icon-512.png --out $P/apple-touch-icon.png
sips -z 32 32 $P/icon-512.png --out $P/favicon-32.png
sips -s format ico $P/favicon-32.png --out $P/favicon.ico
