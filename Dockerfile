FROM golang:1.27-alpine AS base
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

FROM base AS test
RUN go vet ./... && go test ./...

FROM base AS build
RUN CGO_ENABLED=0 go build -o /out/video-server ./cmd/server

FROM alpine:3.22 AS prod
RUN adduser -D -u 10001 app
USER app
COPY --from=build /out/video-server /usr/local/bin/video-server
EXPOSE 8080
ENTRYPOINT ["video-server"]
