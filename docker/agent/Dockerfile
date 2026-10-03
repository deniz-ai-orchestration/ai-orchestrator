# orch agent image: one throwaway container per agent run.
#
#   docker build -t orch-agent docker/agent
#
# CLI versions match the ones the PC2 fixtures were recorded with
# (testdata/fixtures). Bump them together with new fixtures.
ARG GO_VERSION=1.23

# The Go toolchain comes from the official image, so reviewers and testers
# can build and test Go projects. A go.mod that asks for a newer Go makes
# the go command download that toolchain (GOTOOLCHAIN=auto).
FROM golang:${GO_VERSION}-bookworm AS go

FROM node:22-bookworm-slim

ARG CLAUDE_VERSION=2.1.287
ARG CODEX_VERSION=0.160.0
ARG OPENCODE_VERSION=1.18.34

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl git jq make ripgrep \
 && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
      -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
 && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
      > /etc/apt/sources.list.d/github-cli.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends gh \
 && rm -rf /var/lib/apt/lists/*

COPY --from=go /usr/local/go /usr/local/go

RUN npm install -g \
      @anthropic-ai/claude-code@${CLAUDE_VERSION} \
      @openai/codex@${CODEX_VERSION} \
      opencode-ai@${OPENCODE_VERSION} \
 && npm cache clean --force

# orch runs the container as the host user (docker run --user uid:gid), so
# HOME must be writable by any uid. The codex volume is mounted over
# .codex; creating it here gives a new volume the right owner.
RUN mkdir -p /home/agent/.codex /work \
 && chown 1000:1000 /home/agent/.codex \
 && chmod 1777 /home/agent /work

ENV HOME=/home/agent \
    PATH=/usr/local/go/bin:/home/agent/go/bin:$PATH \
    GOPATH=/home/agent/go \
    GOFLAGS=-modcacherw \
    DISABLE_AUTOUPDATER=1 \
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
WORKDIR /work
