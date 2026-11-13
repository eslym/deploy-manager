#!/usr/bin/env bash
set -euo pipefail

deps=/tmp/opencode/opaque-gate-browser-libs
browser=$(bun -e "import { chromium } from 'playwright'; console.log(chromium.executablePath())")
mkdir -p "$deps/debs" "$deps/root"
if ! LD_LIBRARY_PATH="$deps/root/usr/lib/x86_64-linux-gnu" ldd "$browser" | grep -q 'not found'; then
	exec bun run browser-test.mjs
fi

packages=(libatk1.0-0t64 libatk-bridge2.0-0t64 libxcomposite1 libxdamage1 libxfixes3 libxrandr2 libgbm1 libasound2t64 libatspi2.0-0t64 libxkbcommon0 libwayland-client0 libwayland-server0 libdrm2 libxcb1 libx11-xcb1 libxrender1 libxi6 libxres1)
(cd "$deps/debs" && apt-get download "${packages[@]}")
for deb in "$deps"/debs/*.deb; do dpkg-deb -x "$deb" "$deps/root"; done
export LD_LIBRARY_PATH="$deps/root/usr/lib/x86_64-linux-gnu${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
exec bun run browser-test.mjs
