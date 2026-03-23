# Piping Server (Go)

Piping Server (Go) is a small Go implementation of Piping Server designed for simple Docker deployments.

## Supported protocol

- `POST /path` sends data
- `GET /path` receives data
- `GET /path?n=3` and `POST /path?n=3` enable fan-out to multiple receivers
- `GET /help`, `GET /version`, `GET /health` are available as utility endpoints

Transfers stay streamed between the sender and receiver(s), but the sender only gets one final plain-text response when the transfer succeeds or fails. This keeps the request shape compatible with proxy-managed platforms.

## Design notes

- This implementation is written in Go. The original Piping Server project is implemented in Node/TypeScript.
- Uploads are `POST` only in this server. The original server also accepted `PUT`, which kept `curl -T` compatibility.
- The sender does not receive live progress text during upload. It gets one final plain-text response after success or failure.
- This server does not include a built-in browser UI or CLI flags.
- TLS is expected to be terminated by the hosting platform or reverse proxy.
- The current target is a simple single-instance Docker deployment.

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

The server always binds to `0.0.0.0:$PORT`.

## Headers forwarded to receivers

- `Content-Type`
- `Content-Length`
- `Content-Disposition`
- `X-Piping`

## Original Piping Server

The original Piping Server project is available at https://github.com/nwtgck/piping-server.
