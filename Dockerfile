FROM alpine:3.22
RUN apk add --no-cache ca-certificates

COPY websyncd /usr/local/bin/websyncd
RUN chmod +x /usr/local/bin/websyncd

ENTRYPOINT ["/usr/local/bin/websyncd"]
