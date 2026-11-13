# Two stages: a static build, then a small runtime image.
#
# CGO is off, which matters: the SQLite driver is modernc.org/sqlite, a pure-Go
# translation of SQLite. That removes libc and the cgo toolchain from the
# runtime image entirely, so the binary runs on a musl base with no shared
# objects to find. The estate's other Go services use mattn/go-sqlite3 with cgo;
# this one does not need to, and the smaller dependency surface is worth the
# difference in driver.
FROM golang:1.23-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/aprs-archive-collector ./cmd/aprs-archive-collector \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/aprs-archive-web ./cmd/aprs-archive-web

FROM alpine:3.21

# ca-certificates is required: the collector connects to APRS-IS over TCP, and
# the web image may later be fronted by TLS. tzdata keeps local-time formatting
# possible; the aprsarchive itself stores and renders UTC.
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 10001 aprsarchive \
    && adduser -S -u 10001 -G aprsarchive -H -s /sbin/nologin aprsarchive

COPY --from=build /out/aprs-archive-collector /out/aprs-archive-web /usr/local/bin/

ENV APRS_ARCHIVE_DB=/var/lib/aprs-archive/archive.db

# The volume is owned by the runtime user so the collector can create the
# database on a fresh claim. The provisioner creates the directory as
# root:root 0777, and Kubernetes does not chown a local-path volume, so the
# write path is exercised by the process, not by the mount.
RUN mkdir -p /var/lib/aprs-archive && chown -R 10001:10001 /var/lib/aprs-archive

USER 10001:10001
EXPOSE 8080 8081

ENTRYPOINT ["/usr/local/bin/aprs-archive-web"]
