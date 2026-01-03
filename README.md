# Bitmap to G-Code Converter

A web application that converts bitmap images to G-Code for CNC machines, pen plotters, and laser engravers.

## Features

- **Centerline tracing** using [autotrace](https://github.com/autotrace/autotrace) - extracts single-line paths ideal for plotting
- **G-Code generation** using [svg2gcode](https://github.com/sameer/svg2gcode)
- **Configurable output dimensions** - scale to fit your machine's work area
- **Custom tool on/off commands** - works with pen lifts, laser enable, spindle control, etc.
- **Optional AI image transformation** - convert photos to line art using Google's Gemini API
- **AI result caching** - avoids redundant API calls for the same image/prompt

## Quick Start with Docker

```bash
git clone https://github.com/c-jiph/bitmap-to-gcode.git
cd bitmap-to-gcode
docker-compose up -d --build
```

The application will be available at http://localhost:8000

## Docker Compose Configuration

The default `docker-compose.yml`:

```yaml
version: '3.8'

services:
  bitmap-to-gcode:
    build: .
    image: bitmap-to-gcode:latest
    container_name: bitmap-to-gcode
    restart: unless-stopped
    ports:
      - "8000:8000"
    volumes:
      - ./uploads:/data/uploads
      - ./ai_cache:/data/ai_cache
      - ./ai_cache.db:/data/ai_cache.db
    environment:
      - HOSTNAME=localhost:8000
```

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `HOSTNAME` | `localhost:8000` | Hostname shown in generated download links |
| `DATA_DIR` | `/data` | Base directory for uploads and cache |
| `TEMPLATES_DIR` | `/app/templates` | Directory containing HTML templates |

### Volumes

- `./uploads` - Uploaded images and generated files (organized by job ID)
- `./ai_cache` - Cached AI-generated images
- `./ai_cache.db` - SQLite database for cache metadata

## Usage

1. Upload a bitmap image (PNG, JPG, BMP, etc.)
2. Configure output parameters:
   - **Max Width/Height** - Maximum dimensions in mm
   - **Tool On/Off** - G-Code commands for your machine
3. Optionally enable AI transformation to convert photos to line art
4. Download the generated G-Code file

## Processing Pipeline

1. **Upload** - Image uploaded with configuration parameters
2. **AI Transformation** (optional) - Gemini converts image to clean line art
3. **Autotrace** - Centerline tracing produces SVG with single-line paths
4. **Filter** - White/background paths removed from SVG
5. **Scale** - DPI calculated to fit within max dimensions
6. **svg2gcode** - SVG converted to G-Code with tool commands

## Building Without Docker

Requires:
- Go 1.22+
- autotrace (built from source)
- svg2gcode (built from source via Rust/Cargo)

```bash
CGO_ENABLED=1 go build -o bitmap-to-gcode ./cmd/srv
./bitmap-to-gcode -listen :8000
```

See `Dockerfile` for detailed build steps for the dependencies.

## License

MIT
