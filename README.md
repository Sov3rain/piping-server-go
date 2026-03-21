# Piping Server (Go)

This branch replaces the Node/TypeScript server with a small Go implementation designed for Docker and Railway.

## Supported protocol

- `POST /path` sends data
- `GET /path` receives data
- `GET /path?n=3` and `POST /path?n=3` enable fan-out to multiple receivers
- `GET /help`, `GET /version`, `GET /health` are available as utility endpoints

The transfer stays streamed between sender and receiver(s), but the sender only gets one final plain-text response when the transfer succeeds or fails. That keeps the request shape compatible with proxy-managed platforms such as Railway.

## Differences from the original version

- This branch is a Go rewrite; the original project is implemented in Node/TypeScript.
- Uploads are `POST` only here. The original server also accepted `PUT`, which kept `curl -T` compatibility.
- The sender no longer receives live progress text during upload. It gets one final plain-text response after success or failure.
- The built-in browser UI and CLI flags were removed in this branch.
- Application-managed HTTPS was removed. TLS is expected to be terminated by the hosting platform or reverse proxy.
- The current target is a simple Docker deployment on a single instance, especially for Railway-style hosting.

## Examples

Receive:

```bash
curl http://localhost:8080/hello > hello.txt
```

Send:

```bash
echo 'hello, world' | curl -X POST --data-binary @- http://localhost:8080/hello
```

Send to three receivers:

```bash
curl http://localhost:8080/broadcast?n=3 > out1 &
curl http://localhost:8080/broadcast?n=3 > out2 &
curl http://localhost:8080/broadcast?n=3 > out3 &
curl -X POST --data-binary @file.bin "http://localhost:8080/broadcast?n=3"
```

## Docker

Build:

```bash
docker build -t piping-server-go .
```

Run locally:

```bash
docker run --rm -p 8080:8080 -e PORT=8080 piping-server-go
```

Railway will inject `PORT`; the server always binds to `0.0.0.0:$PORT`.

## Headers forwarded to receivers

- `Content-Type`
- `Content-Length`
- `Content-Disposition`
- `X-Piping`

## Removed from this branch

- CLI flags
- `PUT` uploads
- built-in browser UI
- application-managed HTTPS
