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
  remember(run.id, { href: place?.href ?? "", label: place?.label ?? "", last: "" });
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
 * Follows a run's events from just after the last one this browser rendered:
 * the gateway replays what was missed, then continues live. Yields
 * { id, event, data } with data parsed from JSON; ends after `end` or when
 * signal aborts. Events: status, chunk (the /v1 chunk), error, snapshot
 * (after a gateway restart), end.
 */
export async function* events(id, signal) {
  const headers = {};
  const last = readPlaces()[id]?.last;
  if (last) headers["last-event-id"] = last;
  const response = await api(`${ROOT}/${encodeURIComponent(id)}/events`, { headers, signal });
  const reader = response.body.pipeThrough(new TextDecoderStream()).getReader();
  const parse = sseParser();
  let seen = "";
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      for (const message of parse(value)) {
        if (message.id) seen = message.id;
        let data = message.data;
        try {
          data = JSON.parse(data);
        } catch {
          // Not JSON: hand it over as text.
        }
        yield { ...message, data };
        if (message.event === "end") return;
      }
    }
  } finally {
    // Remember the position once per batch, not per token.
    if (seen) remember(id, { last: seen });
    if (signal?.aborted) reader.cancel().catch(() => {});
  }
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
