FROM golang:1.26.6-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /player .

FROM alpine:3.22
RUN apk upgrade --no-cache && apk add --no-cache ca-certificates ffmpeg postgresql-client tzdata && mkdir -p /data && chown 10001:10001 /data
COPY --from=build /player /usr/local/bin/player
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["player"]
