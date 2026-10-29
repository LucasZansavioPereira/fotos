FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /fotos .

FROM alpine:3.22
RUN apk add --no-cache tzdata ffmpeg
COPY --from=build /fotos /usr/local/bin/fotos
ENV FOTOS_DATA=/data FOTOS_ADDR=:8080
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["fotos"]
