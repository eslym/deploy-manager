#!/usr/bin/env python3
"""Run the RFC vector tests from the exact pinned Bytemare module source."""

import json
import subprocess

MODULE = "github.com/bytemare/opaque"
VERSION = "v0.18.0"

download = subprocess.run(
    ["go", "mod", "download", "-json", f"{MODULE}@{VERSION}"],
    check=True,
    capture_output=True,
    text=True,
)
metadata = json.loads(download.stdout)
if metadata.get("Path") != MODULE or metadata.get("Version") != VERSION:
    raise SystemExit(f"unexpected module metadata: {metadata}")
if metadata.get("Error"):
    raise SystemExit(f"module download failed: {metadata['Error']}")
module_dir = metadata.get("Dir")
if not module_dir:
    raise SystemExit("go mod download did not return a module source directory")

print(f"Running pinned RFC vector suite from {module_dir} ({MODULE}@{VERSION})", flush=True)
subprocess.run(["go", "test", "./..."], cwd=module_dir, check=True)
