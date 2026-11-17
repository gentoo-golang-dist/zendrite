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


def disk(radius: int) -> np.ndarray:
    y, x = np.ogrid[-radius : radius + 1, -radius : radius + 1]
    return x * x + y * y <= radius * radius


def render_lines() -> np.ndarray:
    png = bytes(resvg_py.svg_to_bytes(svg_path=str(SOURCE), width=2 * SIZE, height=2 * SIZE))
    lines = Image.open(io.BytesIO(png)).convert("RGBA")
    # The artwork is taller than wide, so crop to the ink and fit its long side.
    lines = lines.crop(lines.getbbox())
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
    }
    for name, image in outputs.items():
        path = PUBLIC / name
        image.save(path, optimize=True)
        print(f"wrote {path.relative_to(ROOT)} ({SIZE}x{SIZE})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
