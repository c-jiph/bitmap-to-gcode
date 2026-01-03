# Stage 1: Build autotrace
FROM debian:bookworm AS autotrace-builder

RUN apt-get update && apt-get install -y \
    git build-essential libpng-dev libexif-dev libpstoedit-dev \
    intltool autoconf automake libtool pkg-config autopoint \
    libmagickcore-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /build
# Clone full repo and use latest release tag (0.31.10)
RUN git clone https://github.com/autotrace/autotrace.git && \
    cd autotrace && \
    git checkout 0.31.10
WORKDIR /build/autotrace
RUN ./autogen.sh && ./configure --prefix=/usr/local && make -j$(nproc) && make install

# Stage 2: Build svg2gcode from source (specific commit for version 0.0.17)
FROM rust:1-bookworm AS svg2gcode-builder

RUN apt-get update && apt-get install -y git && rm -rf /var/lib/apt/lists/*

WORKDIR /build
RUN git clone https://github.com/sameer/svg2gcode.git && \
    cd svg2gcode && \
    git checkout cli-v0.0.17 && \
    cargo build --release -p svg2gcode-cli && \
    cp target/release/svg2gcode /usr/local/bin/

# Stage 3: Build Go application
FROM golang:1.22-bookworm AS go-builder

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -o bitmap-to-gcode ./cmd/srv

# Stage 4: Runtime image
FROM debian:bookworm-slim

# Install runtime dependencies for autotrace
RUN apt-get update && apt-get install -y \
    libpng16-16 libexif12 libpstoedit0c2a \
    libmagickcore-6.q16-6 libgomp1 \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Copy autotrace and its library
COPY --from=autotrace-builder /usr/local/bin/autotrace /usr/local/bin/
COPY --from=autotrace-builder /usr/local/lib/libautotrace* /usr/local/lib/

# Copy svg2gcode
COPY --from=svg2gcode-builder /usr/local/bin/svg2gcode /usr/local/bin/

# Copy Go binary
COPY --from=go-builder /build/bitmap-to-gcode /app/bitmap-to-gcode

# Copy templates
COPY srv/templates /app/templates

# Update library cache
RUN ldconfig

# Create data directory
RUN mkdir -p /data/uploads /data/ai_cache

WORKDIR /app

# Environment variables
ENV DATA_DIR=/data
ENV TEMPLATES_DIR=/app/templates
ENV HOSTNAME=localhost:8000

EXPOSE 8000

CMD ["/app/bitmap-to-gcode", "-listen", ":8000"]
