# Host platform detection
UNAME_S:=$(shell uname)
IS_DARWIN:=$(if $(filter Darwin, $(UNAME_S)),true,false)

# When using OSX/Darwin, you might need to enable CGO for local builds.
DEFAULT_CGO_FLAG:=0
ifeq ($(IS_DARWIN),true)
    DEFAULT_CGO_FLAG:=1
endif
CGO_FLAG?=${DEFAULT_CGO_FLAG}

HOST_OS:=$(shell go env GOOS)
HOST_ARCH:=$(shell go env GOARCH)

GOOS?=${HOST_OS}
GOARCH?=${HOST_ARCH}

DIST_DIR?=dist
# CLI binary name (what we publish as a release artifact)
CLI_NAME?=argocd-resource-tracker
# Backwards-compatible alias
BINNAME?=${CLI_NAME}

CURRENT_DIR=$(shell pwd)
# Build metadata (allow override via env, otherwise compute)
VERSION:=$(if $(VERSION),$(VERSION),$(shell cat ${CURRENT_DIR}/VERSION 2>/dev/null || echo "dev"))
GIT_COMMIT:=$(if $(GIT_COMMIT),$(GIT_COMMIT),$(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown"))
BUILD_DATE:=$(if $(BUILD_DATE),$(BUILD_DATE),$(shell date -u +'%Y-%m-%dT%H:%M:%SZ'))
LDFLAGS:=-w -s -X main.Version=${VERSION} -X main.GitCommit=${GIT_COMMIT} -X main.BuildDate=${BUILD_DATE}

# Space-separated list of GOOS/GOARCH pairs to build for binary releases.
# Intended for GitHub CI release workflows (uploading build artifacts).
RELEASE_PLATFORMS?=linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64
RELEASE_OUTDIR?=${DIST_DIR}/release

# Prefer sha256sum, fall back to shasum (macOS).
SHA256SUM?=$(shell if command -v sha256sum >/dev/null 2>&1; then echo sha256sum; else echo "shasum -a 256"; fi)

.PHONY: all
all: build

.PHONY: clean
clean:
	rm -rf vendor/ ${DIST_DIR}/

.PHONY: mod-tidy
mod-tidy:
	go mod tidy

.PHONY: mod-download
mod-download:
	go mod download

.PHONY: build
build:
	mkdir -p ${DIST_DIR}
	CGO_ENABLED=${CGO_FLAG} GOOS=${GOOS} GOARCH=${GOARCH} go build -ldflags "${LDFLAGS}" -o ${DIST_DIR}/${CLI_NAME} cmd/*.go

.PHONY: test
test:
	go test -coverprofile coverage.out `go list ./... | grep -vE '(test|mocks|vendor)'`

.PHONY: lint
lint:
	golangci-lint run

.PHONY: release-binary
release-binary:
	@if [ -z "${GOOS}" ] || [ -z "${GOARCH}" ]; then \
		echo "GOOS and GOARCH must be set (e.g. make release-binary GOOS=linux GOARCH=amd64)"; \
		exit 2; \
	fi
	@mkdir -p ${RELEASE_OUTDIR}
	@ext=""; \
	if [ "${GOOS}" = "windows" ]; then ext=".exe"; fi; \
	echo ">> Building ${CLI_NAME} for ${GOOS}/${GOARCH}"; \
	CGO_ENABLED=0 GOOS=${GOOS} GOARCH=${GOARCH} go build -trimpath -ldflags "${LDFLAGS}" -o ${RELEASE_OUTDIR}/${CLI_NAME}-${GOOS}_${GOARCH}$$ext cmd/*.go

.PHONY: release-binaries
release-binaries:
	@set -e; \
	for p in ${RELEASE_PLATFORMS}; do \
	  goos=$${p%/*}; \
	  goarch=$${p#*/}; \
	  $(MAKE) release-binary GOOS=$$goos GOARCH=$$goarch; \
	done

.PHONY: release-checksums
release-checksums: release-binaries
	@cd ${RELEASE_OUTDIR} && ${SHA256SUM} ${CLI_NAME}-* > ${CLI_NAME}-${VERSION}-checksums.txt

.PHONY: release
release: release-checksums