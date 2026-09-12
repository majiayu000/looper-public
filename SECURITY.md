# Security Policy

Do not open a public issue for secrets, credential exposure, account takeover
risk, or vulnerabilities that could affect users before a fix is available.

Use GitHub private vulnerability reporting or contact the repository owner
privately.

This project runs local commands from configuration. Treat workflow files as
trusted input and review them before running Looper.

## Observer hardening

- The HTTP observer listens on `127.0.0.1` by default (`-listen` / `-port`).
- `POST /run` is fail-closed: it requires `-auth-token` or `LOOPER_AUTH_TOKEN`.
- Prefer `LOOPER_AUTH_TOKEN` so the secret is not exposed via process argv.
- The dashboard does not embed the shared token in `GET /` HTML; operators supply
  it client-side when triggering runs.
- Dashboard HTML attribute sinks escape quotes so stored content cannot break out
  of `title` attributes and read `localStorage` credentials.
- Cleartext HTTP with an auth token on a non-loopback bind is refused; terminate
  TLS on a reverse proxy to loopback, or opt in with `-allow-insecure-remote`.
- Pass `-enable-run=false` to disable manual job triggers entirely.
- Prefer loopback binds. Exposing the observer beyond localhost increases risk
  even with a shared token.

