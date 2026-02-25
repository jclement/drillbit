# Build stage
FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build arguments for version info
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}" \
    -o /drillbit .

# Runtime stage
FROM alpine:3.23

RUN apk add --no-cache openssh-client pgcli

COPY --from=builder /drillbit /usr/local/bin/drillbit

ENTRYPOINT ["drillbit"]
