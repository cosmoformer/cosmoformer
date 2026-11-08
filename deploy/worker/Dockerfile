# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
ARG BASE_IMAGE=debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2
FROM golang:1.27.3-trixie@sha256:af1bfc2614ebb1dbd31af514b78740fd678850e76498762b425ecee4c5c6ea80 AS build
ARG TARGETARCH
RUN test "$TARGETARCH" = amd64
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -o /out/cosmoformer-worker ./cmd/worker \
 && CGO_ENABLED=0 go test -c -o /out/source.test ./internal/source \
 && CGO_ENABLED=0 go test -c -o /out/worker.test ./internal/worker \
 && CGO_ENABLED=0 go test -c -o /out/clock.test ./internal/clock \
 && CGO_ENABLED=0 go test -c -o /out/command.test ./cmd/worker

FROM ${BASE_IMAGE} AS engine
ARG TARGETARCH
RUN test "$TARGETARCH" = amd64
RUN rm -f /etc/apt/sources.list.d/debian.sources \
 && printf 'deb [check-valid-until=no] http://snapshot.debian.org/archive/debian/20261105T000000Z trixie main\n' > /etc/apt/sources.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl unzip 'git=1:2.47.3-0+deb13u1' \
 && test "$(dpkg-query -W '-f=${Version}' git)" = '1:2.47.3-0+deb13u1'
ARG ENGINE=opentofu
ARG VERSION=1.13.8
ARG ARCHIVE_SHA256=aa4382a5eefcf026309d07398ea803f97b9218a17d940999a938be22bfe9d73f
ARG ENGINE_VERSION
ARG ENGINE_URL
ARG ENGINE_SHA256
ARG ENGINE_MEMBER
RUN set -eu; \
 VERSION=${ENGINE_VERSION:-$VERSION}; ARCHIVE_SHA256=${ENGINE_SHA256:-$ARCHIVE_SHA256}; \
 case "$ENGINE:$VERSION:$ARCHIVE_SHA256" in \
 'opentofu:1.13.8:aa4382a5eefcf026309d07398ea803f97b9218a17d940999a938be22bfe9d73f') \
   binary=tofu; url=https://github.com/opentofu/opentofu/releases/download/v1.13.8/tofu_1.13.8_linux_amd64.zip ;; \
 'terraform:1.17.1:4638628058e71a55212b1c563712eac03b902128f40740f3dc8174df12135061') \
   binary=terraform; url=https://releases.hashicorp.com/terraform/1.17.1/terraform_1.17.1_linux_amd64.zip ;; \
 *) exit 1 ;; esac; \
 test "${ENGINE_URL:-$url}" = "$url"; test "${ENGINE_MEMBER:-$binary}" = "$binary"; \
 curl --fail --location --proto '=https' --proto-redir '=https' --max-time 120 "$url" -o /tmp/runtime.zip; \
 printf '%s  /tmp/runtime.zip\n' "$ARCHIVE_SHA256" | sha256sum -c -; \
 unzip -j /tmp/runtime.zip "$binary" -d /usr/local/bin; \
 chmod 0755 "/usr/local/bin/$binary"; \
 mkdir -p /usr/local/share/cosmoformer /var/lib/cosmoformer/attempts; \
 binary_sha256=$(sha256sum "/usr/local/bin/$binary"); binary_sha256=${binary_sha256%% *}; \
 printf '{"engine":"%s","version":"%s","arch":"amd64","binary_sha256":"%s","git_package_version":"1:2.47.3-0+deb13u1","git_version":"git version 2.47.3"}\n' "$ENGINE" "$VERSION" "$binary_sha256" > /usr/local/share/cosmoformer/runtime.json; \
 rm -f /tmp/runtime.zip; apt-get purge -y curl unzip; rm -rf /var/lib/apt/lists/*; \
 groupadd --gid 10001 cosmoformer; useradd --uid 10001 --gid 10001 --no-create-home cosmoformer; \
 chown 10001:10001 /var/lib/cosmoformer/attempts
USER 10001:10001

FROM engine AS runtime
COPY --from=build /out/cosmoformer-worker /usr/local/bin/cosmoformer-worker
ENTRYPOINT ["/usr/local/bin/cosmoformer-worker"]

FROM runtime AS source-tests
ENV COSMOFORMER_TEST_SOURCE=1
COPY --from=build /out/source.test /out/worker.test /out/clock.test /out/command.test /tests/
ENTRYPOINT ["/bin/sh", "-ec", "for suite in source worker clock command; do /tests/$suite.test -test.v -test.timeout=90s; done"]

FROM build AS coverage-build
RUN CGO_ENABLED=0 go test -c -covermode=atomic -coverpkg=./internal/...,./cmd/... -o /out/source.cover.test ./internal/source \
 && CGO_ENABLED=0 go test -c -covermode=atomic -coverpkg=./internal/...,./cmd/... -o /out/worker.cover.test ./internal/worker \
 && CGO_ENABLED=0 go test -c -covermode=atomic -coverpkg=./internal/...,./cmd/... -o /out/clock.cover.test ./internal/clock \
 && CGO_ENABLED=0 go test -c -covermode=atomic -coverpkg=./internal/...,./cmd/... -o /out/command.cover.test ./cmd/worker

FROM runtime AS source-coverage
ENV COSMOFORMER_TEST_SOURCE=1
COPY --from=coverage-build /out/*.cover.test /tests/
ENTRYPOINT ["/bin/sh", "-ec", "for suite in source worker clock command; do /tests/$suite.cover.test -test.v -test.timeout=90s -test.coverprofile=/coverage/$suite.out; done"]

FROM build AS native-test-build
COPY Makefile /fixtures/Makefile
COPY tests ./tests
COPY deploy/worker/runtimes.json /fixtures/runtimes.json
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c -tags=integration -o /out/acceptance.test ./tests/acceptance
# Build-only fixture supply reads the reviewed manifest and verifies every zip
# before extracting exact members. Nothing is downloaded by the runtime suite.
ARG ENGINE=opentofu
RUN COSMOFORMER_TEST_NATIVE_SUPPLY=1 COSMOFORMER_TEST_NATIVE_ENGINE=$ENGINE \
 COSMOFORMER_TEST_NATIVE_PROFILE_FILE=/fixtures/runtimes.json \
 COSMOFORMER_TEST_NATIVE_PACKAGE_DIR=/out/provider-mirror \
 /out/acceptance.test -test.run='^TestNative(FixtureSupply|MakeRecipe)$' -test.v

FROM runtime AS native-tests
COPY --from=native-test-build /out/acceptance.test /usr/local/bin/cosmoformer-native-tests
COPY --from=native-test-build /out/provider-mirror /fixtures/provider-mirror
COPY deploy/worker/runtimes.json /fixtures/runtimes.json
ENTRYPOINT ["/usr/local/bin/cosmoformer-native-tests", "-test.run=^TestNativeEngineCompatibility$", "-test.v", "-test.timeout=20m"]

# Local tooling depends only on reviewed public packages, never application code.
FROM engine AS local-provider-build
USER root
RUN apt-get update && apt-get install -y --no-install-recommends curl unzip
RUN curl --fail --location --proto '=https' --proto-redir '=https' --max-time 120 \
    https://releases.hashicorp.com/terraform-provider-aws/6.41.0/terraform-provider-aws_6.41.0_linux_amd64.zip -o /tmp/aws.zip \
 && echo '4e8de5f3fcc3e3f41b6703ad4c0fa62175d0c29afcf56ac82eb16c604bb6dafa  /tmp/aws.zip' | sha256sum -c - \
 && mkdir -p /mirror/registry.opentofu.org/hashicorp/aws/6.41.0/linux_amd64 \
 && unzip -j /tmp/aws.zip terraform-provider-aws_v6.41.0_x5 LICENSE.txt -d /mirror/registry.opentofu.org/hashicorp/aws/6.41.0/linux_amd64 \
 && chmod 755 /mirror/registry.opentofu.org/hashicorp/aws/6.41.0/linux_amd64/terraform-provider-aws_v6.41.0_x5

FROM engine AS local-tofu
COPY --from=local-provider-build /mirror /fixtures/provider-mirror
# Retain the original mirror bytes so existing dependency locks stay valid.
COPY deploy/local/aws-package.json /fixtures/provider-mirror/registry.opentofu.org/hashicorp/aws/6.41.0/linux_amd64/cosmoformer-package.json
ENTRYPOINT ["/usr/local/bin/tofu"]

FROM runtime AS worker
