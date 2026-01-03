# Bitmap to G-Code Converter

## Project Overview

A web application that converts bitmap images to G-Code for CNC machines, pen plotters, and laser engravers. It uses **autotrace** for centerline tracing and **svg2gcode** for G-Code generation.

**Live URL**: https://bitmap-to-gcode.exe.xyz:8000/

## Architecture

- **Language**: Go
- **Server**: Simple HTTP server using `net/http`
- **Templates**: HTML templates in `srv/templates/`
- **Uploads**: Stored in `uploads/` directory, organized by job ID
- **Deployment**: Docker container (preferred) or systemd service

## Key Files

```
/home/exedev/bitmap-to-gcode/
├── cmd/srv/main.go          # Entry point
├── srv/
│   ├── server.go            # Main server logic, job processing
│   ├── cache.go             # AI image caching with SQLite
│   └── templates/
│       ├── index.html       # Upload form
│       └── job.html         # Job status/results page
├── uploads/                  # Job directories (created at runtime)
├── ai_cache/                 # Cached AI-generated images
├── ai_cache.db              # SQLite database for cache metadata
├── Dockerfile               # Multi-stage Docker build
├── docker-compose.yml       # Docker Compose configuration
├── .dockerignore            # Docker build exclusions
├── srv.service              # Systemd unit file (legacy)
└── AGENT.md                 # This file
```

## External Tools

Both tools were built from source:

### autotrace (v0.40.0)
- **Location**: `/usr/local/bin/autotrace`
- **Source**: Built from https://github.com/autotrace/autotrace
- **Purpose**: Converts bitmap images to SVG using centerline tracing
- **Key flags used**: `-centerline -color-count 2`

### svg2gcode (v0.0.17)
- **Location**: `/usr/local/bin/svg2gcode`
- **Source**: Built from https://github.com/sameer/svg2gcode
- **Purpose**: Converts SVG paths to G-Code

## Processing Pipeline

1. **Upload**: User uploads image with dimension/tool parameters
2. **AI Transformation (Optional)**: If enabled, use Gemini API to convert image to line art
3. **autotrace**: `autotrace -centerline -color-count 2 -output-file output.svg input.png`
4. **Filter white paths**: Remove paths with stroke color near white (#f0f0f0+) from SVG
5. **Calculate scaling**: Compute DPI to fit output within max dimensions
6. **svg2gcode**: `svg2gcode --on '<tool_on>' --off '<tool_off>' --dpi <dpi> output.svg -o output.gcode`

## Important Discoveries

### 1. Go html/template rejects data URLs as unsafe
When trying to embed images using data URLs (e.g., `data:image/png;base64,...`) in `<img src>` attributes, Go's `html/template` package replaces them with `#ZgotmplZ` (a safe error placeholder). This happens because data URLs are considered potentially unsafe.

**Solution**: Serve images via a proper HTTP endpoint instead of embedding as data URLs. AI-generated images are served from `/ai-cache/{filename}`.

### 2. autotrace outputs white paths for background
When using `-color-count 2`, autotrace traces both the foreground AND background. The background paths have near-white stroke colors (e.g., `#fefefe`). These must be filtered out before passing to svg2gcode, otherwise they appear in the G-Code output.

**Solution**: Regex-based filtering in `filterWhitePaths()` removes `<path>` elements with stroke colors where R, G, and B are all > 240.

### 3. svg2gcode --dimensions does NOT scale output
The `--dimensions` flag only overrides the SVG's declared dimensions - it does NOT scale the coordinate output. The actual coordinates in the G-Code remain in the SVG's native units.

**Solution**: Use `--dpi` flag to control scaling. Calculate DPI as:
```
DPI = (svg_pixels / desired_mm) * 25.4
```

For example, an 832px wide SVG that should output 50mm wide needs:
```
DPI = (832 / 50) * 25.4 = 422.66
```

### 4. SVG dimensions are in pixels (unitless)
autotrace outputs SVG with `width` and `height` attributes as plain numbers (pixels), not with units like `mm`. The `getSVGDimensions()` function parses these.

### 5. Aspect ratio preservation
When scaling to fit within max dimensions, calculate scale factors for both dimensions and use the smaller one:
```go
scaleW := maxW / srcW
scaleH := maxH / srcH
scale := min(scaleW, scaleH)
```

### 6. SQLite requires CGO
The `github.com/mattn/go-sqlite3` package requires CGO to be enabled for compilation:
```bash
CGO_ENABLED=1 go build -o bitmap-to-gcode ./cmd/srv
```

### 7. SQLite schema migrations
When adding columns to existing SQLite tables, use a migration pattern:
1. Check if old schema exists (e.g., check primary key column)
2. Rename old table to `_old` suffix
3. Create new table with updated schema
4. Copy/transform data from old to new table
5. Drop old table

This is implemented in `migrateOldSchema()` in `cache.go`.

## Configuration Options (Web UI)

| Option | Default | Description |
|--------|---------|-------------|
| Max Width | 200 mm | Maximum X dimension of output |
| Max Height | 200 mm | Maximum Y dimension of output |
| Tool On | `S4 M0` | G-Code to turn tool on |
| Tool Off | `S4 M100` | G-Code to turn tool off |
| Use AI | Off | Enable AI image transformation |
| Gemini API Key | - | Required when AI is enabled |
| AI Prompt | (default) | Custom prompt for AI transformation |

All user settings are stored in browser localStorage for persistence across sessions.

## Deployment

### Docker (Preferred)

The application runs in a Docker container that includes all dependencies (autotrace, svg2gcode).

```bash
# Build the image
docker build -t bitmap-to-gcode:latest .

# Run with docker-compose (recommended)
docker-compose up -d

# Or run directly with docker
docker run -d \
  --name bitmap-to-gcode \
  -p 8000:8000 \
  -v ./uploads:/data/uploads \
  -v ./ai_cache:/data/ai_cache \
  -v ./ai_cache.db:/data/ai_cache.db \
  -e HOSTNAME=bitmap-to-gcode.exe.xyz:8000 \
  bitmap-to-gcode:latest

# View logs
docker logs -f bitmap-to-gcode

# Restart after code changes
docker-compose down && docker-compose build && docker-compose up -d
```

#### Docker Image Details

The Dockerfile uses a multi-stage build:
1. **autotrace-builder**: Builds autotrace from source (tag 0.31.10)
2. **svg2gcode-builder**: Builds svg2gcode from source (tag cli-v0.0.17)
3. **go-builder**: Compiles the Go application with CGO
4. **Runtime**: Debian bookworm-slim with minimal runtime dependencies

Environment variables:
- `DATA_DIR`: Base directory for uploads, cache, and database (default: `/data`)
- `TEMPLATES_DIR`: Directory containing HTML templates (default: `/app/templates`)
- `HOSTNAME`: Hostname shown in generated links (default: `localhost:8000`)

### Systemd (Legacy)

```bash
# View status
sudo systemctl status bitmap-to-gcode

# Restart after code changes
CGO_ENABLED=1 go build -o bitmap-to-gcode ./cmd/srv && sudo systemctl restart bitmap-to-gcode

# View logs
journalctl -u bitmap-to-gcode -f
```

## Dependencies Installed

Build dependencies for autotrace:
```
build-essential libpng-dev libexif-dev libpstoedit-dev intltool autoconf automake libtool pkg-config autopoint
```

Rust (for svg2gcode):
```
~/.cargo/bin/cargo, rustc
```

## AI Image Transformation

### Overview
The optional AI transformation stage uses Google's Gemini API to convert uploaded images into clean line art suitable for vectorization. This is useful for:
- Converting photos to line drawings
- Simplifying complex images for plotting
- Creating coloring-book style output

### Implementation Details
- **Model**: `gemini-2.0-flash-exp` (supports image generation)
- **API Endpoint**: `https://generativelanguage.googleapis.com/v1beta/models/gemini-2.0-flash-exp:generateContent`
- **Prompt**: User-customizable, with default that transforms image to two-color line art
- **Default prompt**: "Reduce this image to a two color line-art image suitable for use in a child's coloring book. The lines should be black and the background white. The image will be reproduced by an X-Y plotter, so the final image should have only lines (no solid/filled areas)."
- **Timeout**: 120 seconds (image generation can be slow)

### Security
- **API key handling**: The API key is:
  - Entered by the user in the browser
  - Stored in browser localStorage (never on server)
  - Sent in the form POST to the server
  - Passed directly to Google's API (not stored or logged)
  - Never included in job logs or persisted anywhere on the server

- **localStorage keys used**:
  - `bitmap2gcode_apiKey` - Gemini API key
  - `bitmap2gcode_aiPrompt` - Custom AI prompt
  - `bitmap2gcode_maxWidth` - Max width setting
  - `bitmap2gcode_maxHeight` - Max height setting  
  - `bitmap2gcode_toolOn` - Tool on G-Code
  - `bitmap2gcode_toolOff` - Tool off G-Code
  - `bitmap2gcode_useAI` - AI enabled flag

### Caching
AI-generated images are cached to avoid redundant API calls:

- **Database**: `ai_cache.db` (SQLite) stores mapping of input hash + prompt to cached image
- **Cache directory**: `ai_cache/` stores the actual image files
- **Cache key**: Combination of input image SHA256 hash and prompt hash (first 16 chars)
- **Hash algorithm**: SHA256 of input image file + SHA256 of prompt text
- **Cache lookup**: On each AI transformation request, the input and prompt are hashed and checked against the cache
- **Cache hit**: Returns cached image immediately, logs "Cache HIT"
- **Cache miss**: Calls Gemini API, stores result in cache, logs "Cache MISS"
- **Schema migration**: Old cache entries (without prompt) are automatically migrated with the default prompt

Database schema:
```sql
CREATE TABLE ai_image_cache (
    cache_key TEXT PRIMARY KEY,      -- input_hash:prompt_hash
    input_hash TEXT NOT NULL,        -- SHA256 of input image
    prompt TEXT NOT NULL,            -- Full prompt text
    output_filename TEXT NOT NULL,
    mime_type TEXT NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
```

### Output
When AI transformation is enabled:
- The AI-generated image is saved in `ai_cache/` directory
- It is served via `/ai-cache/{filename}` endpoint
- The job status page shows the image and indicates if it was served from cache
- The transformed image is used as input for autotrace (not the original upload)

## Build & Deploy

### Using Docker (Preferred)

```bash
cd /home/exedev/bitmap-to-gcode

# Build and start
docker-compose up -d --build

# Check status
docker-compose ps
docker-compose logs -f
```

### Manual Build (without Docker)

```bash
# Build (requires CGO for SQLite)
cd /home/exedev/bitmap-to-gcode
CGO_ENABLED=1 go build -o bitmap-to-gcode ./cmd/srv

# Run directly
./bitmap-to-gcode -listen :8000
```

## Future Improvements to Consider

- Add feedrate option (currently hardcoded F300 by svg2gcode)
- Support for begin/end G-Code sequences
- Preview of G-Code toolpaths
- Job persistence across server restarts
- Cleanup of old upload directories
- Cache expiration/cleanup policy
