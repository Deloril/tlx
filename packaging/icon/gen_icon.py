#!/usr/bin/env python3
"""Generate the tlx app icon and its platform variants.

The motif is a forensic timeline: a horizontal axis with event markers rising
from it, coloured in the app's own tag palette (red = bad, amber = suspicious,
green = good). The tallest marker — the find you care about — carries a focus
ring. Drawn at 4x and downsampled so edges stay smooth at any size.

Outputs (run from the repo root):
  internal/gui/icon.png        512x512 master, embedded in the binary
  packaging/windows/tlx.ico     multi-size Windows icon
  packaging/darwin/tlx.icns     macOS bundle icon

No external tools required, only Pillow.
"""

import os
import struct

from PIL import Image, ImageDraw

# Palette. Background is the app's dark slate; markers are the tag colours.
BG_TOP = (38, 50, 56)      # #263238
BG_BOTTOM = (15, 22, 26)   # #0F161A
AXIS = (255, 255, 255, 70)
STEM = (255, 255, 255, 40)
RED = (229, 57, 53)        # Bad
AMBER = (253, 216, 53)     # Suspicious
GREEN = (67, 160, 71)      # Good


def lerp(a, b, t):
    return tuple(round(a[i] + (b[i] - a[i]) * t) for i in range(3))


def rounded_mask(size, radius):
    m = Image.new("L", (size, size), 0)
    d = ImageDraw.Draw(m)
    d.rounded_rectangle([0, 0, size - 1, size - 1], radius=radius, fill=255)
    return m


def vertical_gradient(size, top, bottom):
    grad = Image.new("RGB", (1, size))
    for y in range(size):
        grad.putpixel((0, y), lerp(top, bottom, y / (size - 1)))
    return grad.resize((size, size))


def draw_icon(S):
    """Draw the icon on an S x S RGBA canvas."""
    img = Image.new("RGBA", (S, S), (0, 0, 0, 0))

    # Rounded-square slate background with a vertical gradient.
    bg = vertical_gradient(S, BG_TOP, BG_BOTTOM).convert("RGBA")
    bg.putalpha(rounded_mask(S, radius=round(0.223 * S)))
    img.alpha_composite(bg)

    d = ImageDraw.Draw(img)

    # Timeline axis.
    axis_y = round(0.63 * S)
    x0, x1 = round(0.15 * S), round(0.85 * S)
    half = round(0.014 * S)
    d.rounded_rectangle([x0, axis_y - half, x1, axis_y + half],
                        radius=half, fill=AXIS)

    # Event markers: (x fraction, height above axis fraction, colour).
    markers = [
        (0.24, 0.13, GREEN),
        (0.38, 0.22, AMBER),
        (0.52, 0.33, RED),    # the find — tallest, gets a focus ring
        (0.66, 0.19, AMBER),
        (0.78, 0.11, GREEN),
    ]
    stem_w = round(0.011 * S)
    dot_r = round(0.052 * S)

    for xf, hf, colour in markers:
        cx = round(xf * S)
        cy = axis_y - round(hf * S)
        # Stem from the axis up to the marker.
        d.rounded_rectangle([cx - stem_w, cy, cx + stem_w, axis_y + half],
                            radius=stem_w, fill=STEM)
        # A small notch on the axis under the marker.
        d.ellipse([cx - half, axis_y - half, cx + half, axis_y + half],
                  fill=(255, 255, 255, 110))
        # Focus ring on the red marker.
        if colour == RED:
            ring = round(dot_r * 1.7)
            d.ellipse([cx - ring, cy - ring, cx + ring, cy + ring],
                      outline=colour + (170,), width=round(0.014 * S))
        # The marker dot, with a soft white highlight sitting inside it.
        d.ellipse([cx - dot_r, cy - dot_r, cx + dot_r, cy + dot_r], fill=colour)
        hl = round(dot_r * 0.30)
        hx, hy = cx - round(dot_r * 0.33), cy - round(dot_r * 0.33)
        d.ellipse([hx - hl, hy - hl, hx + hl, hy + hl], fill=(255, 255, 255, 95))

    return img


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
    scale = 4
    master = draw_icon(512 * scale).resize((512, 512), Image.LANCZOS)

    png_path = os.path.join(root, "internal", "gui", "icon.png")
    os.makedirs(os.path.dirname(png_path), exist_ok=True)
    master.save(png_path)

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
    hi = draw_icon(512 * scale)
    for ostype, px in icns_types.items():
        buf = hi.resize((px, px), Image.LANCZOS)
        import io
        b = io.BytesIO()
        buf.save(b, format="PNG")
        png_by_type[ostype] = b.getvalue()
    icns_path = os.path.join(root, "packaging", "darwin", "tlx.icns")
    build_icns(png_by_type, icns_path)

    print("wrote", png_path)
    print("wrote", ico_path)
    print("wrote", icns_path)


if __name__ == "__main__":
    main()
