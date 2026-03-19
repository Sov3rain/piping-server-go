# Railway Transfer Fix

## Summary

This fork keeps the core Piping behavior:

- the sender uploads data with `POST /path` or `PUT /path`
- the receiver downloads data with `GET /path`
- the transfer between sender and receiver is still streamed

The main change is not about removing streaming between sender and receiver.
The main change is about removing the **server-to-sender progress stream** that existed during upload.

That change was made specifically to improve compatibility with Railway's managed HTTP stack, where the original duplex behavior is likely to trigger `ERR_HTTP2_PROTOCOL_ERROR`.

## What the original server did

In the upstream behavior, the sender did not receive a single final response only at the end.

Instead, while the upload request was still in progress, the server wrote text messages back to the sender response, such as:

```text
[INFO] Waiting for 1 receiver(s)...
[INFO] A receiver was connected.
[INFO] Start sending to 1 receiver(s)!
[INFO] Sent successfully!
[INFO] All receiver(s) was/were received successfully.
```

This means the sender connection was effectively used in **duplex mode**:

- request body flowing from sender to server
- response body flowing from server back to sender at the same time

That is different from a simple upload followed by one final response.

## Why this was a problem on Railway

Railway sits behind managed HTTP infrastructure and browser clients typically reach it through HTTP/2.

The original behavior is risky in that environment because it asks the platform and the client path to support:

1. an incoming streaming request body
2. an outgoing streaming response body
3. both happening on the same request at the same time

That is the part most likely to break behind a managed proxy or edge layer.

In other words, the issue was not that sender-to-receiver transfer was streamed.
The issue was that the sender request was also being treated like a live progress channel.

That duplex pattern is a reasonable candidate for causing the `ERR_HTTP2_PROTOCOL_ERROR` you saw on Railway.

## What I changed in the transfer flow

I changed the sender-side response model.

### Before

- `POST` / `PUT` started uploading data
- the server wrote progress text back to the sender while the upload was still happening
- the sender connection acted as both upload channel and live status channel

### Now

- `POST` / `PUT` still uploads data normally
- the server still streams uploaded data to the waiting `GET` receiver(s)
- the sender no longer receives progress messages during upload
- the sender receives **one final plain-text response** only when the transfer completes or fails

Examples of the final sender response:

```text
[INFO] Transfer completed to 1 receiver(s).
```

or

```text
[ERROR] All receiver(s) aborted before completion.
```

## What did not change

The actual transfer path is still streamed.

That means:

- the sender body is still read as a stream
- the server still pipes that stream to the receiver response
- the receiver still gets data progressively instead of waiting for the whole payload to be buffered first

So this is **not** a switch to file buffering or temporary storage.

The change only removes the live progress stream sent back to the uploader.

## Why this should work better on Railway

The new design is much simpler for the HTTP layer:

- sender -> server: upload stream
- server -> receiver: download stream
- server -> sender: one final response when done

This avoids using the sender HTTP response as a second real-time stream during upload.

That matters because a managed HTTP/2 environment is much more likely to accept:

- a normal streaming upload
- plus a final response after completion

than:

- a streaming upload
- plus a simultaneous streaming response on the same request

So the fix targets the most platform-sensitive part of the original design without removing the core sender-to-receiver streaming behavior.

## Additional changes made around this fix

To match the new transfer model, I also updated the browser UI:

- it no longer expects progress text from the server response during upload
- it shows local upload progress from the browser
- once upload is finished, it waits for the final server response

I also added clearer lifecycle logging around:

- sender connected
- receiver connected
- transfer started
- sender end / abort
- receiver finish / abort
- final success or failure

## Practical result

After these changes:

- the app still behaves like a Piping-style transfer server
- receiver-side streaming is preserved
- sender-side duplex progress streaming is removed
- the request shape is more compatible with Railway's HTTP infrastructure

So this is a Railway-focused transfer fix, not a removal of streaming itself.
