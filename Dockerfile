# syntax=docker/dockerfile:1
#
# A public, read-only quill: a static binary in a distroless image serving a
# sample shop database that was built when the image was.
#
#   docker run -p 15433:15433 quill
#   psql -h localhost -p 15433 -U quill

FROM golang:1-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd cmd
COPY internal internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/quill ./cmd/quill
COPY deploy/seed.sql /seed.sql
RUN mkdir /data && /out/quill /data/shop.db < /seed.sql > /dev/null \
 &&/out/quill -c "SELECT COUNT(*) FROM readings" /data/shop.db

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/quill /quill
COPY --from=build --chown=nonroot:nonroot /data /data
WORKDIR /data
EXPOSE 15433
# A database anyone can reach: -read-only refuses every write, so the shared
# data stays as it was built; the timeout and row cap stop one careless query
# (a big self-join, SELECT * over a large table) from stalling everyone else.
ENTRYPOINT ["/quill", "-serve", ":15433", "-read-only", "-statement-timeout", "3s", "-max-rows", "1000", "/data/shop.db"]
