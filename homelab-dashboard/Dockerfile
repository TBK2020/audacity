FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/labdeck ./cmd/labdeck

FROM alpine:3.20
# ping is used by the icmp check type; tzdata for local timestamps in logs.
RUN apk add --no-cache iputils-ping tzdata ca-certificates
WORKDIR /data
COPY --from=build /out/labdeck /usr/local/bin/labdeck
EXPOSE 8383
VOLUME ["/data"]
ENTRYPOINT ["labdeck"]
CMD ["-config", "/data/services.yaml", "-db", "/data/labdeck.db"]
