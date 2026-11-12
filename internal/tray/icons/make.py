# /// script
# requires-python = ">=3.10"
# dependencies = ["resvg-py", "pillow"]
# ///
"""Draws the three tray icons from mic.svg, and the exe carries the first.
Run from the repository root, then pack the icon for the exe:

    uv run internal/tray/icons/make.py
    go run github.com/akavel/rsrc@v0.10.2 -arch amd64 -ico internal/tray/icons/listening.ico -o rsrc_windows_amd64.syso

mic.svg is the studio microphone (U+1F399) from Google's Noto Emoji as it was
drawn until 2021 (commit 546287c772 of googlefonts/noto-emoji), unchanged,
under the Apache License 2.0: see LICENSE-noto-emoji. The later design is a
tilted retro microphone that reads as a blob at sixteen pixels.
"""

import io
import pathlib
import struct

import resvg_py
from PIL import Image, ImageOps

here = pathlib.Path(__file__).parent
mic = (here / "mic.svg").read_text()

# Every size is drawn from the vector, not scaled down from the biggest: at
# sixteen pixels that is the difference between a microphone and a smudge.
sizes = (16, 20, 24, 32, 40, 48, 64, 256)


def listening(im):
    return im


def paused(im):
    # The microphone is grey already, so a pause cannot be told by colour:
    # it fades instead, the way a disabled control does.
    faded = im.copy()
    faded.putalpha(im.getchannel("A").point(lambda a: a * 2 // 5))
    return faded


def alert(im):
    # The same microphone in red: the shape says granag, the colour says
    # something is wrong.
    shade = ImageOps.autocontrast(ImageOps.grayscale(im.convert("RGB")))
    red = ImageOps.colorize(shade, black=(120, 0, 0), white=(255, 110, 100)).convert("RGBA")
    red.putalpha(im.getchannel("A"))
    return red


def ico(images):
    out = struct.pack("<HHH", 0, 1, len(images))
    offset = 6 + 16 * len(images)
    for n, data in images:
        out += struct.pack("<BBBBHHII", n % 256, n % 256, 0, 0, 1, 32, len(data), offset)
        offset += len(data)
    return out + b"".join(data for _, data in images)


drawn = {n: Image.open(io.BytesIO(bytes(resvg_py.svg_to_bytes(svg_string=mic, width=n, height=n)))).convert("RGBA") for n in sizes}
for state in (listening, paused, alert):
    images = []
    for n, im in drawn.items():
        buf = io.BytesIO()
        state(im).save(buf, "PNG")
        images.append((n, buf.getvalue()))
    (here / f"{state.__name__}.ico").write_bytes(ico(images))
