# /// script
# requires-python = ">=3.11"
# dependencies = ["resvg-py", "pillow", "numpy", "scipy"]
# ///

import io
import sys
from pathlib import Path

import numpy as np
import resvg_py
from PIL import Image
from scipy import ndimage

ROOT = Path(__file__).resolve().parent.parent
SOURCE = ROOT / "website" / "src" / "assets" / "logo-light.svg"
PUBLIC = ROOT / "website" / "public"

SIZE = 1024
# Fraction of the canvas reserved as transparent margin on each side.
MARGIN = 0.06
# Radius in px used to bridge small gaps between branches before filling holes.
CLOSE_RADIUS = 40
# Width in px of the white outline drawn around the silhouette.
OUTLINE_RADIUS = 16
# Fraction of the circle's diameter left empty around the art in the Matrix avatar.
CIRCLE_MARGIN = 0.04

# Badges marking the Matrix room avatars: room name, badge colour and Lucide icon (ISC licence) on a 24px grid.
ROOM_BADGES = {
    "announcements": ("#e8590c", '<path d="m3 11 18-5v12L3 14v-3z"/><path d="M11.6 16.8a3 3 0 1 1-5.8-1.6"/>'),
    "development": ("#1971c2", '<path d="m18 16 4-4-4-4"/><path d="m6 8-4 4 4 4"/><path d="m14.5 4-5 16"/>'),
    # The bare question mark is small on the grid, so enlarge it about the grid's centre.
    "help": (
        "#2f9e44",
        '<g transform="translate(12 12) scale(1.6) translate(-12 -13)">'
        '<path d="M9.09 9a3 3 0 0 1 5.83 1c0 2-3 3-3 3"/><path d="M12 17h.01"/></g>',
    ),
}
# Badge radius and the width of the white ring separating it from the art, in px.
BADGE_RADIUS = 180
BADGE_RING = 24
# Direction of the badge from the centre, in degrees below the horizontal.
BADGE_ANGLE = 45
# The art is shrunk by this factor and moved up and left by this many px to make room for the badge.
BADGE_ART_SCALE = 0.8
BADGE_ART_SHIFT = (40, 40)

# Matrix space banner: size in px, and the fonts tried in order for its text.
BANNER_SIZE = (1500, 500)
BANNER_FONTS = "Inter, 'Noto Sans', 'DejaVu Sans', sans-serif"
BANNER_TITLE = "Zendrite"
BANNER_TAGLINE = "A Matrix homeserver written in Go"


def disk(radius: int) -> np.ndarray:
    y, x = np.ogrid[-radius : radius + 1, -radius : radius + 1]
    return x * x + y * y <= radius * radius


def render_art() -> Image.Image:
    png = bytes(resvg_py.svg_to_bytes(svg_path=str(SOURCE), width=2 * SIZE, height=2 * SIZE))
    lines = Image.open(io.BytesIO(png)).convert("RGBA")
    return lines.crop(lines.getbbox())


def render_lines(fit_circle: bool = False) -> np.ndarray:
    # The artwork is taller than wide, so crop to the ink and fit its long side.
    lines = render_art()
    if fit_circle:
        # Matrix clients crop avatars to a circle, so fit the farthest ink from the centre inside it.
        ys, xs = np.nonzero(np.asarray(lines)[:, :, 3] > 25)
        reach = np.hypot(xs - lines.width / 2, ys - lines.height / 2).max()
        scale = SIZE * (1 - 2 * CIRCLE_MARGIN) / (2 * float(reach))
    else:
        inner = SIZE * (1 - 2 * MARGIN) - 2 * OUTLINE_RADIUS
        scale = inner / max(lines.size)
    lines = lines.resize((round(lines.width * scale), round(lines.height * scale)), Image.LANCZOS)
    canvas = Image.new("RGBA", (SIZE, SIZE), (0, 0, 0, 0))
    canvas.paste(lines, ((SIZE - lines.width) // 2, (SIZE - lines.height) // 2))
    return np.asarray(canvas)[:, :, 3].astype(np.float32) / 255.0


def silhouette(alpha: np.ndarray) -> np.ndarray:
    ink = alpha > 0.1
    closed = ndimage.binary_dilation(ink, structure=disk(CLOSE_RADIUS))
    filled = ndimage.binary_fill_holes(closed)
    filled = ndimage.binary_erosion(filled, structure=disk(CLOSE_RADIUS), border_value=0)
    filled |= ink
    sticker = ndimage.binary_dilation(filled, structure=disk(OUTLINE_RADIUS))
    # Soften the outer edge so the downscaled avatar stays anti-aliased.
    return ndimage.gaussian_filter(sticker.astype(np.float32), sigma=1.2)


def tinted(alpha: np.ndarray, value: int) -> Image.Image:
    rgb = np.full((*alpha.shape, 3), value, dtype=np.uint8)
    a = (alpha * 255).round().astype(np.uint8)[:, :, None]
    return Image.fromarray(np.concatenate([rgb, a], axis=2), "RGBA")


def on_white(alpha: np.ndarray) -> Image.Image:
    lum = ((1 - alpha) * 255).round().astype(np.uint8)
    return Image.fromarray(lum, "L").convert("RGB")


def with_badge(base: Image.Image, colour: str, icon: str) -> Image.Image:
    # Sit the badge at the lower right, touching the inside of the circular crop.
    reach = SIZE / 2 * (1 - 2 * CIRCLE_MARGIN) - BADGE_RADIUS - BADGE_RING
    cx = SIZE / 2 + reach * np.cos(np.radians(BADGE_ANGLE))
    cy = SIZE / 2 + reach * np.sin(np.radians(BADGE_ANGLE))
    scale = BADGE_RADIUS * 1.1 / 24
    svg = (
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{SIZE}" height="{SIZE}">'
        f'<circle cx="{cx}" cy="{cy}" r="{BADGE_RADIUS + BADGE_RING}" fill="#fff"/>'
        f'<circle cx="{cx}" cy="{cy}" r="{BADGE_RADIUS}" fill="{colour}"/>'
        f'<g transform="translate({cx - 12 * scale} {cy - 12 * scale}) scale({scale})" fill="none" '
        f'stroke="#fff" stroke-width="2.25" stroke-linecap="round" stroke-linejoin="round">{icon}</g>'
        "</svg>"
    )
    badge = Image.open(io.BytesIO(bytes(resvg_py.svg_to_bytes(svg_string=svg)))).convert("RGBA")
    art = base.resize((round(SIZE * BADGE_ART_SCALE),) * 2, Image.LANCZOS)
    offset = round(SIZE * (1 - BADGE_ART_SCALE) / 2)
    out = Image.new("RGBA", (SIZE, SIZE), (255, 255, 255, 255))
    out.paste(art, (offset - BADGE_ART_SHIFT[0], offset - BADGE_ART_SHIFT[1]))
    out.alpha_composite(badge)
    return out.convert("RGB")


def banner() -> Image.Image:
    width, height = BANNER_SIZE
    art = render_art()
    scale = height * 0.84 / art.height
    art = art.resize((round(art.width * scale), round(art.height * scale)), Image.LANCZOS)
    left = round(height * 0.3)
    text_x = left + art.width + round(height * 0.16)
    svg = (
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="{height}">'
        f'<text x="{text_x}" y="{height * 0.52}" font-family="{BANNER_FONTS}" font-size="{height * 0.3}" '
        f'font-weight="700" fill="#000">{BANNER_TITLE}</text>'
        f'<text x="{text_x + height * 0.01}" y="{height * 0.68}" font-family="{BANNER_FONTS}" '
        f'font-size="{height * 0.085}" fill="#555">{BANNER_TAGLINE}</text>'
        "</svg>"
    )
    text = Image.open(io.BytesIO(bytes(resvg_py.svg_to_bytes(svg_string=svg)))).convert("RGBA")
    out = Image.new("RGBA", BANNER_SIZE, (255, 255, 255, 255))
    out.alpha_composite(art, (left, (height - art.height) // 2))
    out.alpha_composite(text)
    return out.convert("RGB")


def compose(alpha: np.ndarray, backing: np.ndarray) -> Image.Image:
    out_alpha = np.clip(alpha + backing * (1 - alpha), 0, 1)
    # Black ink over a white backing: luminance is the backing's share of the coverage.
    lum = np.where(out_alpha > 0, backing * (1 - alpha) / np.maximum(out_alpha, 1e-6), 0)
    rgb = np.repeat((lum * 255).round().astype(np.uint8)[:, :, None], 3, axis=2)
    a = (out_alpha * 255).round().astype(np.uint8)[:, :, None]
    return Image.fromarray(np.concatenate([rgb, a], axis=2), "RGBA")


def main() -> int:
    alpha = render_lines()
    outputs = {
        # Forge avatar for light themes: black line art.
        "avatar-light.png": tinted(alpha, 0),
        # Forge avatar for dark themes: white line art.
        "avatar-dark.png": tinted(alpha, 255),
        # Single image for places without a theme switch, such as the README.
        "avatar.png": compose(alpha, silhouette(alpha)),
        # Matrix space and room avatar: black line art on opaque white, sized for a circular crop.
        "avatar-matrix.png": on_white(render_lines(fit_circle=True)),
        # Matrix space banner: the logo beside the project name on opaque white.
        "matrix-banner.png": banner(),
    }
    matrix = outputs["avatar-matrix.png"]
    for room, (colour, icon) in ROOM_BADGES.items():
        # Matrix room avatars: the space avatar with a badge naming the room's purpose.
        outputs[f"avatar-matrix-{room}.png"] = with_badge(matrix, colour, icon)
    for name, image in outputs.items():
        path = PUBLIC / name
        image.save(path, optimize=True)
        print(f"wrote {path.relative_to(ROOT)} ({image.width}x{image.height})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
