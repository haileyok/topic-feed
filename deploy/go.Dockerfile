# Builds one of the Go commands under cmd/. Usage (from compose):
#   build: { context: .., dockerfile: deploy/go.Dockerfile, args: { CMD: ingest } }
FROM golang:1.26 AS build
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
