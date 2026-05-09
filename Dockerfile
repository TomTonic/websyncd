FROM golang:1.26-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/websyncd ./cmd/websyncd

FROM alpine:3.22
RUN apk add --no-cache ca-certificates

COPY --from=builder /out/websyncd /usr/local/bin/websyncd

ENTRYPOINT ["/usr/local/bin/websyncd"]
