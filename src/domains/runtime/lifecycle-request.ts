/**
 * Rebuilds an incoming request so handlers see a signal that also fires when
 * the gateway abandons the response.
 *
 * Do not use `new Request(request, { signal })` here. In Bun, copying a server
 * request that way pins its native body stream when no handler reads the body,
 * for example on an auth-rejected POST. Each such request then leaks its body
 * buffer and stream objects for the life of the process. Passing `request.body`
 * explicitly keeps the body streaming and collectable.
 */
export function withLifecycleSignal(
  request: Request,
  signal: AbortSignal,
): Request {
  const body = request.body;
  const init: RequestInit & { duplex?: "half" } = {
    method: request.method,
    headers: request.headers,
    signal,
  };
  if (body) {
    init.body = body;
    init.duplex = "half";
  }
  return new Request(request.url, init);
}
