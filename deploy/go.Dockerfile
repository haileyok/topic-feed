# Builds one of the Go commands under cmd/. Usage (from compose):
#   build: { context: .., dockerfile: deploy/go.Dockerfile, args: { CMD: ingest } }
# golang:1.26.10 is the first release with the crypto/x509 mitigation for CVE-2026-111943
# (the eight eMudhra intermediates revoked for cACompromise in Oct 2026); guarded by
# internal/toolchain. Don't unpin this back to a floating tag.
FROM golang:1.26.10 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG CMD
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${CMD}

# distroless/static includes CA certificates (needed for Jetstream, the AppView, AGW).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
ENTRYPOINT ["/app"]
