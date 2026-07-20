# Eri visual identity

`source/eri-portrait-original.jpg` is original Eri brand artwork supplied by the creator on 2026-07-19. It is not official artwork from the work that inspired Eri and does not imply authorization or endorsement by its rights holders. The creator authorized its use as an Eri-owned project asset under Apache-2.0.

Transparent PNG files are generated deterministically from the original JPEG without generative-model repainting. The process removes only the outer white area connected to the canvas edge and preserves the face, clothing, flowers, and star-shaped negative space inside the ring:

```bash
go run ./scripts/brand-assets
```

- `eri-mark.png`: 1024 px transparent primary mark.
- `eri-icon-512.png` and `eri-icon-192.png`: Web App and desktop-entry icons.
- `eri-favicon-32.png`: browser favicon.

The script also writes the runtime sizes required by the embedded Conversation Workspace and System Observatory assets.
