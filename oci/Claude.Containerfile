FROM docker.io/library/golang:1.26.6-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36 AS claude

ARG TARGETARCH=amd64
ARG CLAUDE_VERSION=2.1.239
RUN case "$TARGETARCH" in \
      amd64) asset_arch=x64; checksum=7de1b1576e2e0be73ce91c2b4dedf16a41058ea633b957a36fdc6044ddfc0f3c ;; \
      arm64) asset_arch=arm64; checksum=66f202c9b52a13318aa7d55e180130fb95ced04af6dc46fd1ea823b598f35556 ;; \
      *) printf 'unsupported TARGETARCH: %s\n' "$TARGETARCH" >&2; exit 1 ;; \
    esac \
    && curl --fail --location --show-error \
      --output /usr/local/bin/claude \
      "https://downloads.claude.ai/claude-code-releases/${CLAUDE_VERSION}/linux-${asset_arch}/claude" \
    && printf '%s  %s\n' "$checksum" /usr/local/bin/claude | sha256sum --check --strict \
    && chmod 0755 /usr/local/bin/claude

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
      /home/cockpit/.claude \
    && install -d -m 0755 /usr/local/share/snowcat-cockpit \
    && ln -s /usr/local/go/bin/go /usr/local/bin/go \
    && ln -s /usr/local/go/bin/gofmt /usr/local/bin/gofmt

COPY --from=claude /usr/local/bin/claude /usr/local/bin/claude
COPY --chmod=0644 oci/claude-mcp.json /usr/local/share/snowcat-cockpit/claude-mcp.json
COPY --chmod=0644 oci/claude-system-prompt.txt /usr/local/share/snowcat-cockpit/claude-system-prompt.txt
COPY --chmod=0755 oci/claude-entrypoint.sh /usr/local/bin/cockpit-entrypoint

ENV HOME=/home/cockpit \
    CLAUDE_CONFIG_DIR=/home/cockpit/.claude \
    GH_CONFIG_DIR=/home/cockpit/.config/gh \
    GOPATH=/home/cockpit/go \
    GOCACHE=/home/cockpit/.cache/go-build

USER 1000:1000
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/cockpit-entrypoint"]
