# Cosmoformer brand assets

The approved **Spiral Ribbon** identity uses two interlocking galaxy arms and the lowercase `cosmoformer` wordmark. The selected ImageGen concept was converted into smooth, flat vector outlines. The wordmark is outlined, so the artwork requires no fonts or external resources.

## Files

- [source.svg](source.svg) is the editable vector master. Its `mark` and `wordmark` groups are the source for every layout.
- [web/public/brand](../../web/public/brand) contains the ready-to-use exports and a file-size manifest.
- [scripts/brand.mjs](../../scripts/brand.mjs) regenerates all exports using Node and the project's existing Playwright Chromium installation.

| Layout | File prefix | Available formats |
| --- | --- | --- |
| Symbol plus name, horizontal | `logo-horizontal-` | SVG; color/reversed PNG at 1600 px wide |
| Symbol above name, stacked | `logo-stacked-` | SVG; color/reversed PNG at 1000 px wide |
| Symbol only, square canvas | `mark-` | SVG; color/reversed transparent PNG at 512 × 512 |
| Name only | `wordmark-` | SVG; color/reversed transparent PNG at 1434 px wide |

Each layout has four SVG variants: `color`, `reversed`, `mono-dark`, and `mono-white`. PNG logo exports have real transparency. SVG exports contain only vector paths, with no embedded bitmap or live text. Prefer SVG for the website and print, and PNG when vector images are unsupported.

## Colors and use

| Color | Value | Purpose |
| --- | --- | --- |
| Forest | `#123c32` | Primary dark arm and wordmark; dark background |
| Emerald | `#167760` | Primary light arm |
| Mint | `#a1caba` | Reversed light arm |
| White | `#ffffff` | Reversed dark arm and wordmark |

Use `color` on a light background and `reversed` on a dark background. Use `mono-dark` or `mono-white` for single-color reproduction. Preserve the aspect ratio and built-in padding. For accessible HTML, give the image `alt="Cosmoformer"` when it supplies the brand name; use an empty alt when adjacent text already names it.

## Browser and app icons

- `favicon.svg`: a rounded forest tile with the reversed symbol, visible on both light and dark browser chrome.
- `favicon.ico`: 16, 32, and 48 px entries; the same sizes are also provided as PNGs.
- `apple-touch-icon.png`: 180 × 180, with an opaque forest background.
- `icon-192.png` and `icon-512.png`: opaque square application icons.

These files provide artwork only; they do not add an installable application or deployment configuration.

## Regenerate

After installing the existing project dependencies and Playwright Chromium:

```sh
make brand
```

This refreshes `web/public/brand` and writes a comparison sheet and a downloadable `cosmoformer-brand.zip` into ignored `.build/brand/`. It needs no database, API key, image-generation request, or global font installation. The app consumes the SVG exports directly.
