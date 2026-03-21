FROM golang:1.23-alpine AS build

WORKDIR /app

COPY go.mod ./
COPY *.go ./

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/piping-server .

FROM alpine:3.20

RUN adduser -D -u 10001 appuser

WORKDIR /app

COPY --from=build /out/piping-server /usr/local/bin/piping-server

ENV PORT=8080

EXPOSE 8080

USER appuser

ENTRYPOINT ["/usr/local/bin/piping-server"]
