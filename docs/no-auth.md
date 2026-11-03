# Running without authentication: `--no-auth`

`local-base serve --no-auth` turns authentication off, so anything that reaches the gateway gets every permission, including the web interface and admin routes.

**It's for local development only. Never tunnel it or put a proxy in front of it.**

## What it refuses

**At startup,** it refuses a gateway host that isn't loopback: `127.0.0.0/8`, `::1`, `localhost`, or a name that resolves only to those. It checks the bound address again after listening.

**On each request,** it answers 403 `no_auth_request_refused` to:

| Request | Routes |
| --- | --- |
| A `Host` other than a loopback address or `localhost` | All |
| Any proxy header: `Forwarded`, `X-Forwarded-*`, `X-Real-Ip`, `Cf-Connecting-Ip`, `Cf-Ray`, `Cf-Access-Jwt-Assertion` | All |
| An `Origin` other than this gateway's, or a `Sec-Fetch-Site` other than `same-origin` or `none` | `/v1/`, `/_localbase/`, `/app/api/`, `/app/session` |

**Why loopback alone isn't enough:** any web page in a browser on your computer can send requests to it, a page can rebind its own name to `127.0.0.1`, and a tunnel on your computer connects from loopback. These rules exist to stop those.

Clients that aren't browsers, such as curl and most SDKs, send no `Origin`, so they work normally.

## What it can't stop

- **A proxy that rewrites `Host` to loopback and strips every proxy header.** A server can't tell that apart from a local request. Don't set one up.
- **Anything else running on your computer as a local client.** It gets full access without a key, including admin routes.

To sign in to the web interface on a real install, use an [identity provider](authentication.md). Authenticated gateways don't apply any of these rules.
