FROM alpine:3.23
RUN apk add --no-cache ca-certificates

ARG TARGETARCH
ARG TARGETVARIANT

COPY dist/ /opt/websyncd-dist/
RUN set -eux; \
    case "${TARGETARCH}${TARGETVARIANT}" in \
      amd64) BIN="websyncd-linux-amd64" ;; \
      arm64) BIN="websyncd-linux-arm64" ;; \
      armv7) BIN="websyncd-linux-armv7" ;; \
      armv6) BIN="websyncd-linux-armv6" ;; \
      *) echo "Unsupported platform: ${TARGETARCH}${TARGETVARIANT}" >&2; exit 1 ;; \
    esac; \
    cp "/opt/websyncd-dist/${BIN}" /usr/local/bin/websyncd; \
    chmod +x /usr/local/bin/websyncd; \
    rm -rf /opt/websyncd-dist

ENTRYPOINT ["/usr/local/bin/websyncd"]
