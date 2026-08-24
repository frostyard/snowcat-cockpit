FROM docker.io/library/node:26-bookworm-slim@sha256:cd565714d4da3e84bfd341e31448f81d47c6362198f152345297c9c1154e6341 AS node

FROM docker.io/library/golang:1.26.7-bookworm@sha256:6ef6e30f0ea5c384f6d111cf856e024e3086bbdcb1779da3f3b3fbba0aea53d2 AS copilot

ARG TARGETARCH=amd64
ARG COPILOT_VERSION=1.0.80
RUN apt-get update \
    && apt-get install --yes --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && case "$TARGETARCH" in \
      amd64) asset_arch=x64; checksum=039933c9247686131c4406abb1d439bdbf68103edc1ff585bd70d5b0dc940f72 ;; \
      arm64) asset_arch=arm64; checksum=3ed85e711955e13be523bf492bc6c93b40b69925bcb7f817c9d08abf4839cf89 ;; \
      *) printf 'unsupported TARGETARCH: %s\n' "$TARGETARCH" >&2; exit 1 ;; \
    esac \
    && curl --fail --location --show-error \
      --output /tmp/copilot.tar.gz \
      "https://github.com/github/copilot-cli/releases/download/v${COPILOT_VERSION}/copilot-linux-${asset_arch}.tar.gz" \
    && printf '%s  %s\n' "$checksum" /tmp/copilot.tar.gz | sha256sum --check --strict \
    && tar --extract --gzip --file /tmp/copilot.tar.gz --directory /usr/local/bin copilot \
    && chmod 0755 /usr/local/bin/copilot

FROM docker.io/library/golang:1.26.7-bookworm@sha256:6ef6e30f0ea5c384f6d111cf856e024e3086bbdcb1779da3f3b3fbba0aea53d2 AS golangci

ARG TARGETARCH=amd64
# The fleet's pinned lint release (std, clix, and updex gates require exactly
# this version). Pinned URL plus per-architecture SHA256 per core ADR-0023.
# Retired once repositories declare tools in mise.lock.
ARG GOLANGCI_LINT_VERSION=2.13.1
RUN case "$TARGETARCH" in \
      amd64) checksum=b17bfbc9d4aaa48be7f4f1ce3240bc3d8200c870c072bacf15c26219e2cfb9cc ;; \
      arm64) checksum=908317c23db18448f924e853b3d8a659fd919614cd438f224810a4053daa2607 ;; \
      *) printf 'unsupported TARGETARCH: %s\n' "$TARGETARCH" >&2; exit 1 ;; \
    esac \
    && curl --fail --location --show-error \
      --output /tmp/golangci-lint.tar.gz \
      "https://github.com/golangci/golangci-lint/releases/download/v${GOLANGCI_LINT_VERSION}/golangci-lint-${GOLANGCI_LINT_VERSION}-linux-${TARGETARCH}.tar.gz" \
    && printf '%s  %s\n' "$checksum" /tmp/golangci-lint.tar.gz | sha256sum --check --strict \
    && tar --extract --gzip --file /tmp/golangci-lint.tar.gz --directory /tmp \
      "golangci-lint-${GOLANGCI_LINT_VERSION}-linux-${TARGETARCH}/golangci-lint" \
    && install -m 0755 "/tmp/golangci-lint-${GOLANGCI_LINT_VERSION}-linux-${TARGETARCH}/golangci-lint" /usr/local/bin/golangci-lint \
    && /usr/local/bin/golangci-lint version --short | grep -qx "$GOLANGCI_LINT_VERSION"

FROM docker.io/library/golang:1.26.7-bookworm@sha256:6ef6e30f0ea5c384f6d111cf856e024e3086bbdcb1779da3f3b3fbba0aea53d2

RUN apt-get update \
    && apt-get install --yes --no-install-recommends \
      bsdextrautils \
      ca-certificates \
      curl \
      gh \
      git \
      jq \
      make \
      openssh-client \
      patch \
      ripgrep \
      unzip \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --home-dir /home/cockpit --shell /bin/bash --uid 1000 cockpit \
    && install -d -o cockpit -g cockpit -m 0700 \
      /home/cockpit/.config/gh \
      /home/cockpit/.copilot \
    && ln -s /usr/local/go/bin/go /usr/local/bin/go \
    && ln -s /usr/local/go/bin/gofmt /usr/local/bin/gofmt

COPY --from=golangci /usr/local/bin/golangci-lint /usr/local/bin/golangci-lint
COPY --from=copilot /usr/local/bin/copilot /usr/local/bin/copilot
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/npm
RUN ln -s /usr/local/lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
    && ln -s /usr/local/lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx
COPY --chmod=0755 oci/copilot-entrypoint.sh /usr/local/bin/cockpit-entrypoint

ENV HOME=/home/cockpit \
    COPILOT_HOME=/home/cockpit/.copilot \
    GH_CONFIG_DIR=/home/cockpit/.config/gh \
    GOPATH=/home/cockpit/go \
    GOCACHE=/home/cockpit/.cache/go-build \
    GOTOOLCHAIN=local

USER 1000:1000
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/cockpit-entrypoint"]
