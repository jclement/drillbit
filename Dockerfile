FROM golang:1.24-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /drillbit .

FROM alpine:3.21
RUN apk add --no-cache openssh-client pgcli
COPY --from=builder /drillbit /usr/local/bin/drillbit
ENTRYPOINT ["drillbit"]
