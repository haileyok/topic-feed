# The live post pipeline (cmd/pipeline). Unlike the other Go services it needs a full
# Debian runtime: it runs the tesseract CLI for image text.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/pipeline

FROM debian:trixie-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends tesseract-ocr tesseract-ocr-eng ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN useradd --system --uid 10001 app
USER app
COPY --from=build /out/app /app
ENTRYPOINT ["/app"]
