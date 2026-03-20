import * as http from "http";
import * as http2 from "http2";
import * as log4js from "log4js";
import * as multiparty from "multiparty";
import * as stream from "stream";
import * as crypto from "crypto";

import * as resources from "./resources";
import {VERSION} from "./version";
import {isReservedPath, NAME_TO_RESERVED_PATH, ReservedPath} from "./reserved-paths";
import * as utils from "./utils";

type HttpReq = http.IncomingMessage | http2.Http2ServerRequest;
type HttpRes = http.ServerResponse | http2.Http2ServerResponse;

type ReqRes = {
  readonly req: HttpReq,
  readonly res: HttpRes
};

type Pipe = {
  readonly sender: ReqRes;
  readonly receivers: ReadonlyArray<ReqRes>;
};

type SenderReqResAndUnsubscribe = {
  readonly req: HttpReq,
  readonly res: HttpRes,
  readonly unsubscribeCloseListener: () => void
}

type ReceiverReqResAndUnsubscribe = {
  readonly req: HttpReq,
  readonly res: HttpRes,
  readonly unsubscribeCloseListener: () => void
};

type UnestablishedPipe = {
  sender?: SenderReqResAndUnsubscribe;
  readonly receivers: ReceiverReqResAndUnsubscribe[];
  readonly nReceivers: number;
};

type Handler = (req: HttpReq, res: HttpRes) => void;

const senderAndReceiverMessageHeaders: Readonly<http.OutgoingHttpHeaders> = {
  "Content-Type": "text/plain",
  "Access-Control-Allow-Origin": "*",
};

function resEndWithContentLength(res: HttpRes, statusCode: number, headers: http.OutgoingHttpHeaders, body: string) {
  writeHead(res, statusCode, {
    "Content-Length": Buffer.byteLength(body),
    ...headers,
  });
  res.end(body);
}

function writeHead(res: HttpRes, statusCode: number, headers?: http.OutgoingHttpHeaders): void {
  (res as any).writeHead(statusCode, headers);
}

function isResponseWritable(res: HttpRes): boolean {
  return !(res as any).destroyed && !(res as any).writableEnded;
}

// Force "HTTP/1.0 ..." response status line, overwriting `req.socket.write`
function forceHttp1_0StatusLine(res: http.ServerResponse) {
  const socket = res.socket!;
  const originalWrite = socket.write;
  socket.write = (chunk: any, ...rest: any) => {
    if (typeof chunk === "string") {
      const replaced = chunk.replace(/^HTTP\/1.1/, "HTTP/1.0");
      // Overwrite socket.write with original one
      socket.write = originalWrite;
      return originalWrite.apply(socket, [replaced, ...rest]);
    }
    return originalWrite.apply(socket, [chunk, ...rest]);
  };
}

/**
 * Convert unestablished pipe to pipe if it is established
 * @param p
 */
function getPipeIfEstablished(p: UnestablishedPipe): Pipe | undefined {
  if (p.sender !== undefined && p.receivers.length === p.nReceivers) {
    return {
      sender: { req: p.sender.req, res: p.sender.res },
      receivers: p.receivers.map((r) => {
        // Unsubscribe on-close handlers
        // NOTE: this operation has side-effect
        r.unsubscribeCloseListener();
        return { req: r.req, res: r.res };
      })
    };
  } else {
    return undefined;
  }
}

export class Server {

  /** Get the number of receivers
   * @param {URL} reqUrl
   * @returns {number}
   */
  private static getNReceivers(reqUrl: URL): number {
    return parseInt(reqUrl.searchParams.get('n') ?? "1", 10)
  }
  private readonly pathToEstablished: Set<string> = new Set();
  private readonly pathToUnestablishedPipe: Map<string, UnestablishedPipe> = new Map();

  /**
   *
   * @param params
   */
  constructor(private params: {
    readonly logger?: log4js.Logger
  } = {}) { }

  public generateHandler(useHttps: boolean): Handler {
    return (req: HttpReq, res: HttpRes) => {
      const reqUrl = new URL(req.url ?? "", "a:///");
      // Get path name
      const reqPath = reqUrl.pathname;
      this.params.logger?.info(`${req.method} ${req.url} HTTP/${req.httpVersion}`);

      // Force "HTTP/1.0 ..." response status line only for HTTP/1.0 client
      if (req.httpVersion === "1.0") {
        forceHttp1_0StatusLine((res as http.ServerResponse));
      }

      if (isReservedPath(reqPath) && (req.method === "GET" || req.method === "HEAD")) {
        this.handleReservedPath(useHttps, req, res, reqPath, reqUrl);
        return;
      }

      switch (req.method) {
        case "POST":
        case "PUT":
          if (isReservedPath(reqPath)) {
            resEndWithContentLength(res, 400, senderAndReceiverMessageHeaders, `[ERROR] Cannot send to the reserved path '${reqPath}'. (e.g. '/mypath123')\n`);
            return;
          }
          // Notify that Content-Range is not supported
          // In the future, resumable upload using Content-Range might be supported
          // ref: https://github.com/httpwg/http-core/pull/653
          if (req.headers["content-range"] !== undefined) {
            resEndWithContentLength(res, 400, senderAndReceiverMessageHeaders, `[ERROR] Content-Range is not supported for now in ${req.method}\n`);
            return;
          }
          // Handle a sender
          this.handleSender(req, res, reqUrl);
          break;
        case "GET":
          // Handle a receiver
          this.handleReceiver(req, res, reqUrl);
          break;
        case "OPTIONS":
          writeHead(res, 200, {
            "Access-Control-Allow-Origin": "*",
            "Access-Control-Allow-Methods": "GET, HEAD, POST, PUT, OPTIONS",
            "Access-Control-Allow-Headers": "Content-Type, Content-Disposition, X-Piping",
            // Private Network Access preflights: https://developer.chrome.com/blog/private-network-access-preflight/
            ...(req.headers["access-control-request-private-network"] === "true" ? {
              "Access-Control-Allow-Private-Network": "true",
            }: {}),
            // Expose "Access-Control-Allow-Headers" for Web browser detecting X-Piping feature
            "Access-Control-Expose-Headers": "Access-Control-Allow-Headers",
            "Access-Control-Max-Age": 86400,
            "Content-Length": 0
          });
          res.end();
          break;
        default:
          resEndWithContentLength(res, 405, {
            "Access-Control-Allow-Origin": "*",
            "Allow": "GET, HEAD, POST, PUT, OPTIONS",
          }, `[ERROR] Unsupported method: ${req.method}.\n`);
          break;
      }
    };
  }

  private handleReservedPath(useHttps: boolean, req: HttpReq, res: HttpRes, reqPath: ReservedPath, reqUrl: URL) {
    switch (reqPath) {
      case NAME_TO_RESERVED_PATH.index:
        resEndWithContentLength(res, 200, {
          "Content-Type": "text/html"
        }, resources.indexPage);
        return;
      case NAME_TO_RESERVED_PATH.noscript: {
        const styleNonce = crypto.randomBytes(16).toString("base64");
        resEndWithContentLength(res, 200, {
          "Content-Type": "text/html",
          "Content-Security-Policy": `default-src 'none'; style-src 'nonce-${styleNonce}'`
        }, resources.noScriptHtml(reqUrl.searchParams, styleNonce));
        return;
      }
      case NAME_TO_RESERVED_PATH.version:
        const versionPage: string = VERSION + "\n";
        resEndWithContentLength(res, 200, {
          "Access-Control-Allow-Origin": "*",
          "Content-Type": "text/plain"
        }, versionPage);
        return;
      case NAME_TO_RESERVED_PATH.help:
        // x-forwarded-proto is https or not
        const xForwardedProtoIsHttps: boolean = (() => {
          const proto = req.headers["x-forwarded-proto"];
          // NOTE: includes() is for supporting Glitch
          return proto !== undefined && proto.includes("https");
        })();
        const scheme: string = (useHttps || xForwardedProtoIsHttps) ? "https" : "http";
        // NOTE: req.headers.host contains port number
        const hostname: string = req.headers.host ?? "hostname";
        // tslint:disable-next-line:no-shadowed-variable
        const url = `${scheme}://${hostname}`;

        const helpPage: string = resources.generateHelpPage(url);
        resEndWithContentLength(res, 200, {
          "Access-Control-Allow-Origin": "*",
          "Content-Type": "text/plain"
        }, helpPage);
        return;
      case NAME_TO_RESERVED_PATH.faviconIco:
        // (from: https://stackoverflow.com/a/35408810/2885946)
        writeHead(res, 204);
        res.end();
        break;
      case NAME_TO_RESERVED_PATH.robotsTxt:
        writeHead(res, 404, {
          "Content-Length": 0,
        });
        res.end();
        return;
    }
  }

  private logLifecycle(path: string, event: string, detail?: string): void {
    const suffix = detail === undefined ? "" : `, ${detail}`;
    this.params.logger?.info(`${event}: path='${path}'${suffix}`);
  }

  private finalizeSenderResponse(sender: ReqRes, path: string, statusCode: number, message: string): void {
    if (isResponseWritable(sender.res)) {
      resEndWithContentLength(sender.res, statusCode, senderAndReceiverMessageHeaders, message);
    } else {
      this.logLifecycle(path, "sender response skipped", `status=${statusCode}`);
    }
    this.removeEstablished(path);
  }

  /**
   * Start data transfer
   *
   * @param path
   * @param pipe
   */
  // tslint:disable-next-line:no-shadowed-variable
  private async runPipe(path: string, pipe: Pipe): Promise<void> {
    // Add to established
    this.pathToEstablished.add(path);
    // Delete unestablished pipe
    this.pathToUnestablishedPipe.delete(path);

    const {sender, receivers} = pipe;
    this.logLifecycle(path, "transfer started", `receivers=${receivers.length}`);

    try {
      const isMultipart: boolean = (sender.req.headers["content-type"] ?? "").includes("multipart/form-data");

      const part: multiparty.Part | undefined =
        isMultipart ?
          await new Promise((resolve, reject) => {
            const form = new multiparty.Form();
            form.once("part", (p: multiparty.Part) => {
              resolve(p);
            });
            form.once("error", (error) => {
              reject(error);
            });
            // TODO: Not use any
            form.parse(sender.req as any);
          }) :
          undefined;

      const senderData: stream.Readable =
        part === undefined ? sender.req : part;

      const contentLength: string | number | undefined = part === undefined ?
        sender.req.headers["content-length"] : part.byteCount;
      // Get Content-Type from part or HTTP header.
      const contentType: string | undefined = (() => {
        const type: string | undefined = (part === undefined ?
          sender.req.headers["content-type"] : part.headers["content-type"]);
        if (type === undefined) {
          return undefined;
        }
        const matched = type.match(/^\s*([^;]*)(\s*;?.*)$/);
        // If invalid Content-Type
        if (matched === null) {
          return undefined;
        } else {
          // Extract MIME type and parameters
          const mimeType: string = matched[1];
          const params: string = matched[2];
          // If it is text/html, it should replace it with text/plain not to render in browser.
          // It is the same as GitHub Raw (https://raw.githubusercontent.com).
          // "text/plain" can be consider a superordinate concept of "text/html"
          return mimeType === "text/html" ? "text/plain" + params : type;
        }
      })();
      const contentDisposition: string | undefined = part === undefined ?
        sender.req.headers["content-disposition"] : part.headers["content-disposition"];
      const parseHeaders = utils.parseHeaders(sender.req.rawHeaders);
      const xPiping: string[] = parseHeaders.get("x-piping") ?? [];

      type ReceiverTransferState = "pending" | "completed" | "aborted";
      const receiverStates: ReceiverTransferState[] = receivers.map(() => "pending");
      let senderResponseFinalized = false;
      let senderEnded = false;

      const finalizeSender = (statusCode: number, message: string): void => {
        if (senderResponseFinalized) {
          return;
        }
        senderResponseFinalized = true;
        this.finalizeSenderResponse(sender, path, statusCode, message);
      };

      const maybeFinalizeSender = (): void => {
        const completedCount = receiverStates.filter((state) => state === "completed").length;
        const abortedCount = receiverStates.filter((state) => state === "aborted").length;
        if (completedCount + abortedCount !== receiverStates.length) {
          return;
        }
        if (abortedCount === 0) {
          this.logLifecycle(path, "transfer completed", `receivers=${completedCount}`);
          finalizeSender(200, `[INFO] Transfer completed to ${completedCount} receiver(s).\n`);
          return;
        }
        if (completedCount === 0) {
          if (!senderEnded) {
            this.logLifecycle(path, "all receivers aborted", "draining sender upload");
            senderData.resume();
            return;
          }
          this.logLifecycle(path, "transfer failed", "all receivers aborted");
          finalizeSender(500, "[ERROR] All receiver(s) aborted before completion.\n");
          return;
        }
        this.logLifecycle(path, "transfer partially failed", `completed=${completedCount}, aborted=${abortedCount}`);
        finalizeSender(500, `[ERROR] Transfer completed for ${completedCount}/${receiverStates.length} receiver(s).\n`);
      };

      receivers.forEach((receiver, index) => {
        // Write headers to a receiver
        writeHead(receiver.res, 200, {
          ...(contentLength === undefined ? {} : {"Content-Length": contentLength}),
          ...(contentType === undefined ? {} : {"Content-Type": contentType}),
          ...(contentDisposition === undefined ? {} : {"Content-Disposition": contentDisposition}),
          "X-Piping": xPiping,
          "Access-Control-Allow-Origin": "*",
          ...(xPiping.length === 0 ? {} : {"Access-Control-Expose-Headers": "X-Piping"}),
          "X-Robots-Tag": "none",
        });

        const passThrough = new stream.PassThrough();
        senderData.pipe(passThrough);
        passThrough.pipe(receiver.res);

        const completeReceiver = (state: ReceiverTransferState, event: string): void => {
          if (receiverStates[index] !== "pending") {
            return;
          }
          receiverStates[index] = state;
          this.logLifecycle(path, event, `receiver=${index + 1}/${receivers.length}`);
          if (state === "aborted") {
            senderData.unpipe(passThrough);
            passThrough.destroy();
          }
          maybeFinalizeSender();
        };

        receiver.res.once("finish", () => {
          completeReceiver("completed", "receiver finished");
        });
        receiver.res.once("close", () => {
          if (!(receiver.res as any).writableFinished) {
            completeReceiver("aborted", "receiver response closed");
            return;
          }
          this.logLifecycle(path, "receiver response closed", `receiver=${index + 1}/${receivers.length}`);
        });
        receiver.req.once("aborted", () => {
          completeReceiver("aborted", "receiver aborted");
        });
        receiver.req.once("error", () => {
          completeReceiver("aborted", "receiver request error");
        });
        receiver.res.once("error", () => {
          completeReceiver("aborted", "receiver response error");
        });
      });

      sender.req.once("close", () => {
        this.logLifecycle(path, "sender request closed");
      });

      senderData.on("close", () => {
        this.logLifecycle(path, "sender stream closed");
      });

      senderData.on("aborted", () => {
        for (const receiver of receivers) {
          // Close a receiver
          if (receiver.res.connection !== undefined && receiver.res.connection !== null) {
            receiver.res.connection.destroy();
          }
        }
        this.logLifecycle(path, "sender aborted");
        if (!senderResponseFinalized) {
          this.removeEstablished(path);
        }
      });

      senderData.on("end", () => {
        senderEnded = true;
        this.logLifecycle(path, "sender stream ended");
        maybeFinalizeSender();
      });

      senderData.on("error", () => {
        for (const receiver of receivers) {
          if (receiver.res.connection !== undefined && receiver.res.connection !== null) {
            receiver.res.connection.destroy();
          }
        }
        this.logLifecycle(path, "sender stream error");
        finalizeSender(500, "[ERROR] Failed to send.\n");
      });
    } catch (error) {
      this.logLifecycle(path, "transfer setup failed", error instanceof Error ? error.message : "unknown error");
      this.finalizeSenderResponse(sender, path, 500, "[ERROR] Failed to start transfer.\n");
    }
  }

  // Delete from established
  private removeEstablished(path: string): void {
    this.pathToEstablished.delete(path);
    this.params.logger?.info(`established '${path}' removed`);
  }

  /**
   * Handle a sender
   * @param {HttpReq} req
   * @param {HttpRes} res
   * @param {URL} reqUrl
   */
  private handleSender(req: HttpReq, res: HttpRes, reqUrl: URL): void {
    const reqPath = reqUrl.pathname;
    // Get the number of receivers
    const nReceivers = Server.getNReceivers(reqUrl);
    if (Number.isNaN(nReceivers)) {
      resEndWithContentLength(res, 400, senderAndReceiverMessageHeaders, `[ERROR] Invalid "n" query parameter\n`);
      return;
    }
    // If the number of receivers is invalid
    if (nReceivers <= 0) {
      resEndWithContentLength(res, 400, senderAndReceiverMessageHeaders, `[ERROR] n should > 0, but n = ${nReceivers}.\n`);
      return;
    }
    if (this.pathToEstablished.has(reqPath)) {
      resEndWithContentLength(res, 400, senderAndReceiverMessageHeaders, `[ERROR] Connection on '${reqPath}' has been established already.\n`);
      return;
    }
    // Get unestablished pipe
    const unestablishedPipe = this.pathToUnestablishedPipe.get(reqPath);
    // If the path connection is not connecting
    if (unestablishedPipe === undefined) {
      // Create a sender
      const sender = this.createSenderOrReceiver("sender", req, res, reqPath);
      // Register new unestablished pipe
      this.pathToUnestablishedPipe.set(reqPath, {
        sender: sender,
        receivers: [],
        nReceivers: nReceivers
      });
      this.logLifecycle(reqPath, "sender connected", `waiting-for=${nReceivers}`);
      return;
    }
    // If a sender has been connected already
    if (unestablishedPipe.sender !== undefined) {
      resEndWithContentLength(res, 400, senderAndReceiverMessageHeaders, `[ERROR] Another sender has been connected on '${reqPath}'.\n`);
      return;
    }
    // If the number of receivers is not the same size as connecting pipe's one
    if (nReceivers !== unestablishedPipe.nReceivers) {
      resEndWithContentLength(res, 400, senderAndReceiverMessageHeaders, `[ERROR] The number of receivers should be ${unestablishedPipe.nReceivers} but ${nReceivers}.\n`);
      return;
    }
    // Register the sender
    unestablishedPipe.sender = this.createSenderOrReceiver("sender", req, res, reqPath);
    this.logLifecycle(reqPath, "sender connected", `receivers=${unestablishedPipe.receivers.length}/${nReceivers}`);
    // Get pipeOpt if established
    const pipe: Pipe | undefined =
      getPipeIfEstablished(unestablishedPipe);

    if (pipe !== undefined) {
      // Start data transfer
      this.runPipe(reqPath, pipe);
    }
  }

  /**
   * Handle a receiver
   * @param {HttpReq} req
   * @param {HttpRes} res
   * @param {URL} reqUrl
   */
  private handleReceiver(req: HttpReq, res: HttpRes, reqUrl: URL): void {
    const reqPath = reqUrl.pathname;
    // If the receiver requests Service Worker registration
    // (from: https://speakerdeck.com/masatokinugawa/pwa-study-sw?slide=32)"
    if (req.headers["service-worker"] === "script") {
      // Reject Service Worker registration
      resEndWithContentLength(res, 400, senderAndReceiverMessageHeaders, `[ERROR] Service Worker registration is rejected.\n`);
      return;
    }
    // Get the number of receivers
    const nReceivers = Server.getNReceivers(reqUrl);
    if (Number.isNaN(nReceivers)) {
      resEndWithContentLength(res, 400, senderAndReceiverMessageHeaders, `[ERROR] Invalid query parameter "n"\n`);
      return;
    }
    // If the number of receivers is invalid
    if (nReceivers <= 0) {
      resEndWithContentLength(res, 400, senderAndReceiverMessageHeaders, `[ERROR] n should > 0, but n = ${nReceivers}.\n`);
      return;
    }
    // The connection has been established already
    if (this.pathToEstablished.has(reqPath)) {
      resEndWithContentLength(res, 400, senderAndReceiverMessageHeaders, `[ERROR] Connection on '${reqPath}' has been established already.\n`);
      return;
    }
    // Get unestablishedPipe
    const unestablishedPipe = this.pathToUnestablishedPipe.get(reqPath);
    // If the path connection is not connecting
    if (unestablishedPipe === undefined) {
      // Create a receiver
      /* tslint:disable:no-shadowed-variable */
      const receiver = this.createSenderOrReceiver("receiver", req, res, reqPath);
      // Set a receiver
      this.pathToUnestablishedPipe.set(reqPath, {
        receivers: [receiver],
        nReceivers: nReceivers
      });
      this.logLifecycle(reqPath, "receiver connected", `receivers=1/${nReceivers}`);
      return;
    }
    // If the number of receivers is not the same size as connecting pipe's one
    if (nReceivers !== unestablishedPipe.nReceivers) {
      resEndWithContentLength(res, 400, senderAndReceiverMessageHeaders, `[ERROR] The number of receivers should be ${unestablishedPipe.nReceivers} but ${nReceivers}.\n`);
      return;
    }
    // If more receivers can not connect
    if (unestablishedPipe.receivers.length === unestablishedPipe.nReceivers) {
      resEndWithContentLength(res, 400, senderAndReceiverMessageHeaders, "[ERROR] The number of receivers has reached limits.\n");
      return;
    }

    // Create a receiver
    const receiver = this.createSenderOrReceiver("receiver", req, res, reqPath);
    // Append new receiver
    unestablishedPipe.receivers.push(receiver);
    this.logLifecycle(reqPath, "receiver connected", `receivers=${unestablishedPipe.receivers.length}/${nReceivers}`);

    // Get pipeOpt if established
    const pipe: Pipe | undefined =
      getPipeIfEstablished(unestablishedPipe);

    if (pipe !== undefined) {
      // Start data transfer
      this.runPipe(reqPath, pipe);
    }
  }

  /**
   * Create a sender or receiver
   *
   * Main purpose of this method is creating sender/receiver which unregisters unestablished pipe before establish
   *
   * @param reqResType
   * @param req
   * @param res
   * @param reqPath
   */
  private createSenderOrReceiver(reqResType: "sender", req: HttpReq, res: HttpRes, reqPath: string): SenderReqResAndUnsubscribe
  private createSenderOrReceiver(reqResType: "receiver", req: HttpReq, res: HttpRes, reqPath: string): ReceiverReqResAndUnsubscribe
  private createSenderOrReceiver(reqResType: "sender" | "receiver", req: HttpReq, res: HttpRes, reqPath: string): SenderReqResAndUnsubscribe | ReceiverReqResAndUnsubscribe {
    // Define on-close handler
    const closeListener = () => {
      // Get unestablished pipe
      const unestablishedPipe = this.pathToUnestablishedPipe.get(reqPath);
      // If the pipe is registered
      if (unestablishedPipe !== undefined) {
        // Get sender/receiver remover
        const remover =
          reqResType === "sender" ?
            (): boolean => {
              // If sender is defined
              if (unestablishedPipe.sender !== undefined) {
                // Remove sender
                unestablishedPipe.sender = undefined;
                return true;
              }
              return false;
            } :
            (): boolean => {
              // Get receivers
              const receivers = unestablishedPipe.receivers;
              // Find receiver's index
              const idx = receivers.findIndex((r) => r.req === req);
              // If receiver is found
              if (idx !== -1) {
                // Delete the receiver from the receivers
                receivers.splice(idx, 1);
                return true;
              }
              return false;
            };
        // Remove a sender or receiver
        const removed: boolean = remover();
        // If removed
        if (removed) {
          // If unestablished pipe has no sender and no receivers
          if (unestablishedPipe.receivers.length === 0 && unestablishedPipe.sender === undefined) {
            // Remove unestablished pipe
            this.pathToUnestablishedPipe.delete(reqPath);
            this.params.logger?.info(`unestablished '${reqPath}' removed`);
          }
        }
      }
    };
    // Disconnect if it close
    req.once("close", closeListener);
    // Unsubscribe "close"
    const unsubscribeCloseListener = () => {
      req.removeListener("close", closeListener);
    };
    if (reqResType === "sender" && req.httpVersion === "1.0") {
      return {
        req: req,
        res: res,
        unsubscribeCloseListener,
      };
    }
    return {
      req: req,
      res: res,
      unsubscribeCloseListener,
    };
  }
}
