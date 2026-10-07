# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/agg ./cmd/agg \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/agg /agg
# owned by the nonroot user, so a fresh Docker volume mounted here is writable
COPY --from=build --chown=65532:65532 /out/data /data
VOLUME /data
ENV AGG_DB=/data/agg.db AGG_ADDR=:8080
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/agg", "serve"]
