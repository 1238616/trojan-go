# Build from THIS repository's source tree (issue #14). The previous
# version cloned upstream p4gefau1t/trojan-go, so the resulting image
# contained none of this fork's features (cluster routing, singmux,
# muxcool, monitoring dashboard, ...).
FROM golang:alpine AS builder
WORKDIR /src
# The full tree (including .git) is copied so the Makefile can embed the
# version/commit stamp via `git describe` / `git rev-parse`.
COPY . .
RUN apk add --no-cache git make &&\
    make &&\
    wget https://github.com/v2fly/domain-list-community/raw/release/dlc.dat -O build/geosite.dat &&\
    wget https://github.com/v2fly/geoip/raw/release/geoip.dat -O build/geoip.dat &&\
    wget https://github.com/v2fly/geoip/raw/release/geoip-only-cn-private.dat -O build/geoip-only-cn-private.dat

FROM alpine
WORKDIR /
RUN apk add --no-cache tzdata ca-certificates
COPY --from=builder /src/build /usr/local/bin/
COPY --from=builder /src/example/server.json /etc/trojan-go/config.json

ENTRYPOINT ["/usr/local/bin/trojan-go", "-config"]
CMD ["/etc/trojan-go/config.json"]
