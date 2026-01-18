FROM golang:1.24-bookworm AS builder

ARG VERSION

WORKDIR /clabernetes

RUN mkdir build

COPY . .

RUN go mod download

RUN CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64 \
    go build \
    -ldflags "-s -w -X github.com/srl-labs/clabernetes/constants.Version=${VERSION}" \
    -trimpath \
    -a \
    -o \
    build/manager \
    cmd/clabernetes/main.go

FROM debian:bookworm-slim

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

ARG CONTAINERLAB_VERSION=0.72.0

RUN apt-get update && \
    apt-get install -yq --no-install-recommends \
    ca-certificates \
    curl \
    wget \
    vim \
    jq \
    iproute2 \
    iptables \
    tcpdump \
    procps \
    ethtool \
    openssh-client \
    inetutils-ping \
    traceroute

# Install containerlab CLI (used for connectivity helpers like VXLAN).
RUN curl -fsSL -o /tmp/containerlab.tgz \
      "https://github.com/srl-labs/containerlab/releases/download/v${CONTAINERLAB_VERSION}/containerlab_${CONTAINERLAB_VERSION}_Linux_amd64.tar.gz" && \
    tar -C /usr/local/bin -xzf /tmp/containerlab.tgz containerlab && \
    chmod +x /usr/local/bin/containerlab && \
    rm -f /tmp/containerlab.tgz
RUN apt-get clean && \
    rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/* /var/cache/apt/archive/*.deb

# copy a basic but nicer than standard bashrc for the user
COPY build/launcher/.bashrc /root/.bashrc

# copy default ssh keys to the launcher image
# to make use of password-less ssh access
COPY build/launcher/default_id_rsa /root/.ssh/id_rsa
COPY build/launcher/default_id_rsa.pub /root/.ssh/id_rsa.pub
RUN chmod 600 /root/.ssh/id_rsa

# copy custom ssh config to enable easy ssh access from launcher
COPY build/launcher/ssh_config /etc/ssh/ssh_config

# copy sshin command to simplify ssh access to the containers
COPY build/launcher/sshin /usr/local/bin/sshin

# copy shellin command to simplify shell access to the containers
COPY build/launcher/shellin /usr/local/bin/shellin

WORKDIR /clabernetes

RUN mkdir .node
RUN mkdir .image

COPY --from=builder /clabernetes/build/manager .
USER root

ENTRYPOINT ["/clabernetes/manager", "launch"]
