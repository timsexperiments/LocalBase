# First-run sign-in link

A fresh install has no identity provider, so LocalBase gives you a one-time link to sign in from a browser on the same computer.

**The link only works from a browser on the same computer. A phone or another computer needs an [identity provider](authentication.md).**

## Getting a link

`local-base start` and foreground `local-base serve` print a link once, on a data directory with no identity provider. To get another:

```sh
local-base access login-link
```

It prints a link like `http://127.0.0.1:2273/app/login/once#<token>`. A link works **once, within 10 minutes**, and issuing a new one replaces any unused one. You can issue a link while the gateway is stopped; start it before opening the link.

If an identity provider is already configured, `login-link` refuses unless you add `--force`, because the session it creates bypasses your role policy.

## The session it creates

- **Permissions:** all of them, on purpose. Whoever can run the CLI already controls the data directory.
- **Lifetime:** 12 hours idle, 7 days at most.
- **Limit:** at most 5 at once. A new one ends the least recently used.
- **Identity:** every local session is the same owner, `local`.

```sh
local-base access sessions list
local-base access sessions revoke <id>
local-base access login-link --revoke     # end every local session and unused link
```

## Letting someone else in

The local session doesn't go through the sign-in policy, so this page's link keeps working whatever the policy says. Someone signing in through a provider does go through it, and with no permissions gets a page saying "This account has no access here", with the command that fixes it. See [troubleshooting](troubleshooting.md#sign-in). `local-base access policy starter --email <address>` gives one address every permission, and nobody else anything.

## What it protects against

- **Someone elsewhere on the network, or arriving through a tunnel or standard reverse proxy,** using a link or a session. They fail the loopback rule on every request.
- **A web page in a browser on this computer.** Rebinding fails the `Host` check, and cross-site requests fail the same-origin rules and the `SameSite=Strict` cookie.
- **A link in a log, a preview or a prefetch.** The token is in the URL fragment, so it isn't sent to any server, and a `GET` never spends it.
- **A copy of the database or a diagnostics bundle being replayed.** Only hashes are stored.
- **Guessing.** Tokens are 32 random bytes, and redeem failures are rate limited.

## What it doesn't protect against

- **Anyone who can run commands as this user, or read the terminal or its scrollback, within the 10 minutes.** They could already run the CLI, which has full control.
- **Another account or process on this computer that gets the link before you use it.** Anything that can reach loopback and holds the token can redeem it.
- **Malware or a browser extension in your browser session.** The session has every permission.
- **Traffic on loopback.** It's plain HTTP, which doesn't leave the machine, but a privileged local process could read it.
- **A proxy that rewrites `Host` to loopback and strips every proxy header.** It looks exactly like a local request. This setup is unsupported in front of a local session.
- **Reviewing who did what.** Audit events say a local session acted, not which person.

It isn't sign-in for other people or devices.

**Not yet confirmed in real browsers.** This is tested at the HTTP level, and `start`'s link hasn't been tested end to end.
