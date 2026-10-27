// Detached runs (DESIGN §21): generation that keeps going on the gateway
// while the person switches tab, page or app. Chat and the Lab create a run,
// then follow its events; on load, or when the tab is shown again, a page
// re-attaches and replays from the last event it rendered. Stop is the only
// cancel. The client side and its fallback are DESIGN §22.4.
//
// Until the gateway serves /_localbase/runs, supported() resolves false and
// callers keep today's direct requests.

const ROOT = "/_localbase/runs";
const PLACES = "localbase.runs.places.v1"; // run id -> { href, label, last }
export const ACTIVE = new Set(["queued", "in_progress"]);
let support;

// A gateway request as the signed-in person, like tools.js api() but without
// its dependencies, so every page can load this module. Errors carry the
// gateway's message; a redirect means the edge sign-in expired.
async function api(path, init = {}) {
  const headers = new Headers(init.headers);
  headers.set("x-localbase-ui", "1");
  const response = await fetch(`/app/api${path}`, { ...init, headers, cache: "no-store", credentials: "same-origin", redirect: "manual" });
  if (response.type === "opaqueredirect") throw new Error("Your sign-in expired. Reload to sign in again.");
  if (!response.ok) {
    const value = await response.json().catch(() => null);
    const error = new Error(value?.error?.message ?? `Request failed (${response.status}). Try again.`);
    error.status = response.status;
    throw error;
  }
  return response;
}

const jsonRequest = (body, signal) => ({ method: "POST", signal, headers: { "content-type": "application/json" }, body: JSON.stringify(body) });

/** Whether the gateway serves detached runs (probed once per page). */
export function supported() {
  support ??= list().then(
    () => true,
    () => false,
  );
  return support;
}

/** The caller's runs, newest first, without output. */
export async function list() {
  const body = await (await api(ROOT)).json();
  const runs = Array.isArray(body) ? body : (body.data ?? []);
  const places = readPlaces();
  return runs.map((run) => ({ ...run, place: places[run.id] ?? null }));
}

/** The caller's runs still queued or in progress. */
export async function active() {
  if (!(await supported())) return [];
  return (await list()).filter((run) => ACTIVE.has(run.status));
}

/**
 * Starts a run of a /v1 route ("chat/completions", "images/generations", …)
 * with exactly that route's body. A validation error throws like /v1 does,
 * and no run exists. place ({ href, label }) is where "tap to return" goes.
 */
export async function create(route, body, place, signal) {
  const init = body instanceof FormData ? { method: "POST", body, signal } : jsonRequest(body, signal);
  const run = await (await api(`${ROOT}/v1/${route}`, init)).json();
  remember(run.id, { ...place, last: "" });
  notify();
  return run;
}

/** Status and the output so far. */
export async function get(id) {
  return (await api(`${ROOT}/${encodeURIComponent(id)}`)).json();
}

/** Stop: cancels the run; its partial output stays until dismissed. */
export async function cancel(id) {
  await api(`${ROOT}/${encodeURIComponent(id)}/cancel`, { method: "POST" });
  notify();
}

/** Dismiss: the page has collected the result; delete the run now. */
export async function dismiss(id) {
  await api(`${ROOT}/${encodeURIComponent(id)}`, { method: "DELETE" });
  forget(id);
  notify();
}

/** A media run's result (image or audio), once it has completed. */
export async function result(id) {
  return (await api(`${ROOT}/${encodeURIComponent(id)}/result`)).blob();
}

/**
 * Follows a run's events: the gateway replays every event after `after`
 * (default: the last one this browser rendered; "" for all), then continues
 * live. Yields { id, event, data, raw } with data parsed from JSON; ends
 * after `end` or when signal aborts. Events: status, chunk (the /v1 chunk),
 * error, snapshot (the run with its output, after a gateway restart), end.
 *
 * A dropped connection (a phone suspending the tab, a network change) is
 * resumed from the last event once the page is visible again, so nothing is
 * lost or repeated.
 */
export async function* events(id, { signal, after } = {}) {
  let last = after ?? readPlaces()[id]?.last ?? "";
  for (let attempt = 0; ; attempt++) {
    let response;
    try {
      const headers = last ? { "last-event-id": last } : {};
      response = await api(`${ROOT}/${encodeURIComponent(id)}/events`, { headers, signal });
    } catch (error) {
      if (signal?.aborted || error.status || attempt >= reconnectDelaysMs.length) throw error;
      await reconnectPause(attempt, signal);
      continue;
    }
    const reader = response.body.pipeThrough(new TextDecoderStream()).getReader();
    const parse = sseParser();
    let got = false;
    try {
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        for (const message of parse(value)) {
          got = true;
          if (message.id) last = message.id;
          let data = message.data;
          try {
            data = JSON.parse(data);
          } catch {
            // Not JSON: hand it over as text.
          }
          yield { ...message, data, raw: message.data };
          if (message.event === "end") return;
        }
        attempt = 0;
      }
    } catch (error) {
      if (signal?.aborted || attempt >= reconnectDelaysMs.length) throw error;
    } finally {
      // Remember the position once per connection, not per token.
      if (last) remember(id, { last });
      reader.cancel().catch(() => {});
    }
    if (signal?.aborted) return;
    // A resume that brings nothing may follow a gateway restart, after which
    // the run's event ids start again: ask for its state, and end with it
    // if it is over (the page already has the output up to `last`).
    if (!got && last) {
      const run = await get(id).catch(() => null);
      if (run && !ACTIVE.has(run.status)) {
        const { output, ...end } = run;
        yield { id: "", event: "end", data: end, raw: JSON.stringify(end) };
        return;
      }
    }
    // The stream closed or failed before `end`: resume.
    await reconnectPause(attempt, signal);
  }
}

const reconnectDelaysMs = [500, 1000, 2000, 4000, 8000, 15000];

// Waits before reconnecting, and first for the page to be shown again: a
// hidden tab's requests fail or stall on phones.
async function reconnectPause(attempt, signal) {
  if (document.hidden)
    await new Promise((resolve) => document.addEventListener("visibilitychange", function shown() {
      if (document.hidden) return;
      document.removeEventListener("visibilitychange", shown);
      resolve();
    }));
  await new Promise((resolve) => setTimeout(resolve, reconnectDelaysMs[attempt]));
  signal?.throwIfAborted();
}

/**
 * A run as a /v1 streaming Response, for code that reads chat-completions
 * streams (chat-core.js streamText): chunk events become `data:` lines, a
 * completed run ends with [DONE], and an error, cancel or interruption
 * becomes an error event. With `create`, the run is started first
 * ({ route, body, place }); otherwise `id` is followed from `after`.
 * Aborting the signal stops the run on the gateway (Stop); leaving the page
 * does not. onRun(run) gets the run once known; onEnd(run) its final state.
 */
export async function response({ create: start, id, after, signal, onRun, onEnd }) {
  if (start) {
    const run = await create(start.route, start.body, start.place, signal);
    id = run.id;
    onRun?.(run);
  }
  // Stop: the page already holds the partial output, so the run can go.
  const stop = () => cancel(id).then(() => dismiss(id)).catch(() => {});
  signal?.addEventListener("abort", stop, { once: true });
  const encoder = new TextEncoder();
  const line = (data) => encoder.encode(`data: ${data}\n\n`);
  const body = new ReadableStream({
    async start(controller) {
      try {
        let failed = false;
        // What this stream has delivered, so a snapshot adds only the rest.
        const sent = { content: "", reasoning: "" };
        for await (const e of events(id, { signal, after })) {
          if (e.event === "error") {
            failed = true;
            if (e.data?.error?.code === "model_unloading") e.raw = JSON.stringify({ error: { ...e.data.error, message: unloadingMessage } });
          }
          if (e.event === "chunk") {
            const delta = e.data?.choices?.[0]?.delta ?? {};
            sent.content += delta.content ?? "";
            sent.reasoning += delta.reasoning ?? delta.reasoning_content ?? "";
          }
          if (e.event === "chunk" || e.event === "error") controller.enqueue(line(e.raw));
          else if (e.event === "snapshot") {
            const chunk = snapshotChunk(e.data, sent);
            if (chunk) controller.enqueue(line(JSON.stringify(chunk)));
          }
          else if (e.event === "end") {
            onEnd?.(e.data);
            if (e.data?.status === "completed") controller.enqueue(line("[DONE]"));
            else if (!failed)
              controller.enqueue(line(JSON.stringify({ error: { message: endMessage(e.data), code: `run_${e.data?.status}` } })));
          }
        }
        controller.close();
      } catch (error) {
        controller.error(error);
      } finally {
        signal?.removeEventListener("abort", stop);
      }
    },
  });
  return new Response(body, { headers: { "content-type": "text/event-stream" } });
}

// After a gateway restart the event buffer is gone and the snapshot carries
// the output as of the last checkpoint (DESIGN §21.3: it replaces what was
// rendered). As a stream that only appends, it adds what this stream has
// not delivered yet; when the stream is already ahead of the checkpoint
// (or differs from it) it adds nothing.
export function snapshotChunk(run, sent = { content: "", reasoning: "" }) {
  const out = run?.output ?? {};
  const rest = (full = "", had) => (full.startsWith(had) ? full.slice(had.length) : "");
  const content = rest(out.content, sent.content);
  const reasoning = rest(out.reasoning, sent.reasoning);
  if (!content && !reasoning) return null;
  return { choices: [{ index: 0, delta: { ...(content ? { content } : {}), ...(reasoning ? { reasoning } : {}) } }] };
}

// A request that reached a model while someone unloaded it (DESIGN §23).
export const unloadingMessage = "The model was being unloaded, so this didn’t run. Try again: it loads again when needed.";

export function endMessage(run) {
  if (run?.error?.code === "model_unloading") return unloadingMessage;
  if (run?.status === "cancelled") return "Stopped.";
  if (run?.reason === "output_limit") return "The reply grew too long and was stopped.";
  switch (run?.reason) {
    case "gateway_restart":
      return "LocalBase restarted before the reply finished.";
    case "memory_pressure":
      return "The reply stopped because the computer ran low on memory.";
    case "max_duration":
      return "The reply ran for the maximum time and was stopped.";
  }
  return "The reply stopped before it finished.";
}


/** Forget a run's place (its result has been collected or it is gone). */
export function forget(id) {
  const places = readPlaces();
  delete places[id];
  writePlaces(places);
}

/** Tells the shell to refresh its "Generating…" indicator. */
export function notify() {
  document.dispatchEvent(new CustomEvent("localbase:runs"));
}

/**
 * An incremental text/event-stream parser: feed it chunks, get complete
 * messages back. Comments (": ping") are skipped. Exported for the tests.
 */
export function sseParser() {
  let buffer = "";
  return (chunk) => {
    buffer += chunk.replace(/\r\n?/g, "\n");
    const out = [];
    let end;
    while ((end = buffer.indexOf("\n\n")) >= 0) {
      const block = buffer.slice(0, end);
      buffer = buffer.slice(end + 2);
      const message = { id: "", event: "message", data: "" };
      const data = [];
      for (const line of block.split("\n")) {
        if (!line || line.startsWith(":")) continue;
        const colon = line.indexOf(":");
        const field = colon < 0 ? line : line.slice(0, colon);
        const value = colon < 0 ? "" : line.slice(colon + 1).replace(/^ /, "");
        if (field === "data") data.push(value);
        else if (field === "id") message.id = value;
        else if (field === "event") message.event = value;
      }
      if (data.length) out.push({ ...message, data: data.join("\n") });
    }
    return out;
  };
}

function remember(id, patch) {
  const places = readPlaces();
  places[id] = { ...places[id], ...patch };
  writePlaces(places);
}

function readPlaces() {
  try {
    return JSON.parse(localStorage.getItem(PLACES) ?? "{}") ?? {};
  } catch {
    return {};
  }
}

function writePlaces(places) {
  try {
    localStorage.setItem(PLACES, JSON.stringify(places));
  } catch {
    // Storage full or blocked: re-attaching then replays from the start.
  }
}
