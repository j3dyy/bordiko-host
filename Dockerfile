# Standalone image for the Bordiko game-host.
#
# Build context is THIS directory (no go.work dependency), so the service can be
# extracted into its own repo. Games are loaded from the registry on demand
# (REGISTRY_URL) and/or a mounted GAMES_DIR.
FROM golang:1.26-bookworm AS build
ENV CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -o /out/app .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
EXPOSE 8081
ENTRYPOINT ["/app"]
