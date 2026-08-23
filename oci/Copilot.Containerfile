FROM docker.io/library/node:26-bookworm-slim@sha256:cd565714d4da3e84bfd341e31448f81d47c6362198f152345297c9c1154e6341 AS node

FROM docker.io/library/golang:1.26.6-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36 AS copilot

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

FROM docker.io/library/golang:1.26.6-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36

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
    GOCACHE=/home/cockpit/.cache/go-build

USER 1000:1000
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/cockpit-entrypoint"]
