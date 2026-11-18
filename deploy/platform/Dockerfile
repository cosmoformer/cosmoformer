# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
FROM golang:1.27.3-trixie@sha256:af1bfc2614ebb1dbd31af514b78740fd678850e76498762b425ecee4c5c6ea80 AS go-build
# RUN cache entries must be specific to the commit epoch: export only clamps
# newer mtimes and cannot raise older timestamps retained by cached stages.
ARG SOURCE_DATE_EPOCH
ARG TARGETARCH
RUN test "$TARGETARCH" = amd64
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY LICENSE ./
RUN CGO_ENABLED=0 go build -buildvcs=false -trimpath -o /out/cosmoformer ./cmd/cosmoformer \
 && CGO_ENABLED=0 go build -buildvcs=false -trimpath -o /out/contract ./cmd/contract \
 && /out/contract > /out/openapi.json \
 && /out/cosmoformer migrate --list > /out/migrations.txt \
 && mkdir -p /out/licenses/go /out/root/tmp /out/root/run/cosmoformer \
 && chmod 1777 /out/root/tmp && chmod 700 /out/root/run/cosmoformer \
 && chown 10001:10001 /out/root/run/cosmoformer \
 && cp LICENSE /out/licenses/Cosmoformer-MIT \
 && go list -m -f '{{if and .Dir .Version}}{{.Path}} {{.Version}} {{.Dir}}{{end}}' all > /out/modules.txt \
 && while read name version dir; do dest="/out/licenses/go/$name@$version"; mkdir -p "$dest"; find "$dir" -maxdepth 1 -type f \( -iname '*license*' -o -iname '*notice*' -o -iname '*copying*' \) -exec cp -t "$dest" '{}' + || exit 1; done < /out/modules.txt \
 && go version -m /out/cosmoformer > /out/go-build.txt \
 && cd /out && sha256sum cosmoformer openapi.json > SHA256SUMS

FROM node:26.13.0-trixie-slim@sha256:5adad64f9a1fca2d51972caa65983634c8a774eb300d72b20f7f2a0f60368072 AS web-build
ARG SOURCE_DATE_EPOCH
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN test "$(node --version)" = v26.13.0 && npm ci --ignore-scripts
COPY web ./
COPY --from=go-build /out/contract /usr/local/bin/contract
# The normal contract generator consumes this executable instead of a second source build.
ENV COSMOFORMER_CONTRACT_BINARY=/usr/local/bin/contract
COPY deploy/platform/package-web.mjs /src/deploy/platform/package-web.mjs
RUN npm run contract && npm run build && node /src/deploy/platform/package-web.mjs

FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2 AS certificates
ARG SOURCE_DATE_EPOCH
# Same pinned snapshot as the worker; CA identities remain in the package database.
RUN rm -f /etc/apt/sources.list.d/debian.sources \
 && printf 'deb [check-valid-until=no] http://snapshot.debian.org/archive/debian/20261105T000000Z trixie main\n' > /etc/apt/sources.list \
 && apt-get update && apt-get install -y --no-install-recommends ca-certificates

FROM scratch AS platform
# Bind the COPY chain to this source/epoch before copying cached stage outputs.
COPY packaging-source.json /usr/local/share/cosmoformer/source.json
COPY --from=go-build --chown=10001:10001 /out/root/ /
COPY --from=certificates /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=certificates /usr/share/doc/ca-certificates/copyright /usr/local/share/cosmoformer/licenses/ca-certificates
COPY --from=go-build /out/cosmoformer /usr/local/bin/cosmoformer
COPY --from=go-build /out/licenses /usr/local/share/cosmoformer/licenses
COPY --from=go-build /out/openapi.json /out/migrations.txt /out/go-build.txt /out/SHA256SUMS /usr/local/share/cosmoformer/
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/cosmoformer"]
CMD ["serve"]
STOPSIGNAL SIGTERM

FROM nginx:1.28-alpine@sha256:a8b39bd9cf0f83869a2162827a0caf6137ddf759d50a171451b335cecc87d236 AS proxy
ARG SOURCE_DATE_EPOCH
USER root
RUN rm -rf /usr/share/nginx/html /etc/nginx/conf.d /docker-entrypoint.d /docker-entrypoint.sh /var/cache/apk/* \
 && mkdir -p /run/cosmoformer && chown 10001:10001 /run/cosmoformer && chmod 700 /run/cosmoformer
COPY deploy/platform/nginx.conf /etc/nginx/nginx.conf
COPY deploy/platform/proxy.inc deploy/platform/private-access.conf /etc/nginx/
COPY --from=web-build /src/web/build/client /usr/share/nginx/html
COPY --from=web-build /src/web/build/csp.conf /etc/nginx/csp.conf
COPY --from=web-build /src/web/build/SHA256SUMS /usr/local/share/cosmoformer/SHA256SUMS
COPY --from=web-build /src/web/build/licenses /usr/local/share/cosmoformer/licenses/web
COPY --from=go-build /out/licenses/Cosmoformer-MIT /usr/local/share/cosmoformer/licenses/Cosmoformer-MIT
COPY packaging-source.json /usr/local/share/cosmoformer/source.json
USER 10001:10001
EXPOSE 8443
ENTRYPOINT ["nginx"]
CMD ["-g", "daemon off;"]
STOPSIGNAL SIGQUIT

# Disposable verification only. Never used by either deployment reference.
FROM go-build AS browser-build
COPY tests ./tests
RUN CGO_ENABLED=0 go test -c -tags=integration -o /out/browser-fixture ./tests/acceptance \
 && test "$(/out/browser-fixture -test.list=^TestPackagedRestartPreservesUncertainOwnership$)" = TestPackagedRestartPreservesUncertainOwnership
FROM platform AS browser-fixture
COPY --from=browser-build /out/browser-fixture /usr/local/bin/browser-fixture
COPY --from=browser-build /src/internal/testdata /suite/internal/testdata
WORKDIR /suite/tests/acceptance
ENTRYPOINT ["/usr/local/bin/browser-fixture"]
CMD ["-test.run=^TestBrowserServer$", "-test.v", "-test.timeout=30m"]
