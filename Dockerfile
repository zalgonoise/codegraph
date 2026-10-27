# The codegraph toolchain, so any CI runs the tests and the stack analysis the
# same way (see the Makefile's docker-* targets): Go, the cgo dependencies of
# the analysed repositories (pkg-config and libxml2), python3 for a knowledge
# base comparison, helm to render the environment's chart (offline,
# `helm template` only), and make. The repositories, the Go module
# cache and the outputs are mounted at run time, so the image holds neither
# the analysed code nor credentials.
ARG GO_VERSION=1.27
FROM golang:${GO_VERSION}

RUN apt-get update \
	&& apt-get install -y --no-install-recommends pkg-config libxml2-dev python3 make git ca-certificates \
	&& rm -rf /var/lib/apt/lists/* \
	&& git config --system --add safe.directory '*'

# helm, from its release site, checked against the published checksum.
ARG HELM_VERSION=v4.3.0
RUN arch="$(dpkg --print-architecture)" && cd /tmp \
	&& curl -fsSLO "https://get.helm.sh/helm-${HELM_VERSION}-linux-${arch}.tar.gz" \
	&& curl -fsSL "https://get.helm.sh/helm-${HELM_VERSION}-linux-${arch}.tar.gz.sha256sum" | sha256sum -c - \
	&& tar -xzf "helm-${HELM_VERSION}-linux-${arch}.tar.gz" \
	&& mv "linux-${arch}/helm" /usr/local/bin/helm \
	&& rm -rf /tmp/helm-* "/tmp/linux-${arch}"

WORKDIR /codegraph
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# The version it reports and writes into its outputs (make docker-image passes git describe's).
ARG VERSION=devel
RUN go build -ldflags "-X main.version=${VERSION}" -o /usr/local/bin/codegraph ./cmd/codegraph

# The Makefile runs the installed binary instead of go run.
ENV CODEGRAPH=codegraph
CMD ["make", "help"]
