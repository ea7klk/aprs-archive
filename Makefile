.PHONY: build test vet fmt lint run-collector run-web docker clean

VERSION ?= dev
IMAGE ?= ghcr.io/ea7klk/aprs-archive

# Build both binaries into ./build. The two commands are the whole delivery:
# there is no framework, no code generation and no front-end build step.
build:
	go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o build/aprs-archive-collector ./cmd/aprs-archive-collector
	go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o build/aprs-archive-web ./cmd/aprs-archive-web
	go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o build/aprs-archive-admin ./cmd/aprs-archive-admin

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

lint: vet
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "not gofmt'd: $$unformatted"; exit 1; fi

# Run locally against a throwaway archive. `make run-collector` in one terminal
# and `make run-web` in another share ./data.
run-collector:
	APRS_ARCHIVE_DB=$(CURDIR)/data/archive.db \
	APRS_ARCHIVE_HEALTH_ADDR=:8081 \
	go run ./cmd/aprs-archive-collector

run-web:
	APRS_ARCHIVE_DB=$(CURDIR)/data/archive.db \
	APRS_ARCHIVE_ADDR=:8080 \
	go run ./cmd/aprs-archive-web

# The admin server needs a password and its own port; it writes the archive.
run-admin:
	APRS_ARCHIVE_DB=$(CURDIR)/data/archive.db \
	APRS_ARCHIVE_ADDR=:8082 \
	APRS_ARCHIVE_ADMIN_PASSWORD=$${ADMIN_PASSWORD:-devpassword} \
	go run ./cmd/aprs-archive-admin

docker:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

clean:
	rm -rf build
