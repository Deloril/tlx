#!/usr/bin/env python3
"""Derive the platform icon variants from the master app icon.

internal/gui/icon.png is the source of truth: the icon embedded in the binary
and shown on the running window. This script reads it and writes the Windows
and macOS containers from it, so Explorer and Finder show the same art. It does
not draw or overwrite the master.

Outputs (run from the repo root):
  packaging/windows/tlx.ico     multi-size Windows icon
  packaging/darwin/tlx.icns     macOS bundle icon

Only Pillow is required.
"""

import io
import os
import struct

from PIL import Image


def build_icns(png_by_type, path):
    """Assemble a PNG-based .icns container from {ostype: png_bytes}."""
    body = b""
    for ostype, data in png_by_type.items():
        body += ostype + struct.pack(">I", len(data) + 8) + data
    header = b"icns" + struct.pack(">I", len(body) + 8)
    with open(path, "wb") as f:
        f.write(header + body)


def main():
    root = os.getcwd()
    png_path = os.path.join(root, "internal", "gui", "icon.png")
    master = Image.open(png_path).convert("RGBA")

    ico_path = os.path.join(root, "packaging", "windows", "tlx.ico")
    os.makedirs(os.path.dirname(ico_path), exist_ok=True)
    master.save(ico_path, sizes=[(16, 16), (32, 32), (48, 48),
                                 (64, 64), (128, 128), (256, 256)])

    # macOS .icns: PNG-encoded entries at the sizes Finder expects.
    icns_types = {
        b"ic07": 128, b"ic08": 256, b"ic09": 512,
        b"ic11": 32, b"ic12": 64, b"ic13": 256, b"ic14": 512,
    }
    png_by_type = {}
    for ostype, px in icns_types.items():
        buf = master.resize((px, px), Image.LANCZOS)
        b = io.BytesIO()
        buf.save(b, format="PNG")
        png_by_type[ostype] = b.getvalue()
    icns_path = os.path.join(root, "packaging", "darwin", "tlx.icns")
    build_icns(png_by_type, icns_path)

    print("read ", png_path)
    print("wrote", ico_path)
    print("wrote", icns_path)


if __name__ == "__main__":
    main()
