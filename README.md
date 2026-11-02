# tibi-mcp

Go MCP server for searching Tibi e-books, borrowing an exact e-book edition,
requesting delivery through Tibi's existing Kindle service and getting an EPUB
download link for a borrowed book. Remote access goes
through mcp-gateway; the server uses stateless Streamable HTTP at `/mcp`.

## Status

Public search and book detail routes have been checked against tibi.no.
Verified against a real account on 2026-11-02: login with borrower number and
PIN, an authenticated book lookup, and borrowing an e-book (`borrowed: true`
afterwards). `send_to_kindle` reached Tibi, which answered `true`, but the book
did not arrive. Tibi's help page states that Kindle sending is currently broken
("På grunn av en teknisk feil er det ikke mulig å sende e-bøker til Kindle",
updated 2026-08-21). Kindle delivery therefore remains unverified.

## Configuration

| Variable | Meaning |
| --- | --- |
| `MCP_AUTH_TOKEN` | Required bearer token for HTTP, distinct from account credentials |
| `TIBI_CARD_NUMBER` | Tibi borrower number; not required for public searches |
| `TIBI_PIN` | Tibi PIN |
| `MCP_READ_ONLY` | Defaults to `true`; set `false` to expose borrowing and Kindle tools |
| `CONFIG_TOKEN` | Bearer token for `/config` and `/healthz?check=1`; defaults to `MCP_AUTH_TOKEN` |
| `DATA_DIR` | Where settings from the gateway are stored, default `/data` |
| `MCP_TRANSPORT` | `http` (default) or `stdio` |
| `PORT` | HTTP listen port, default `8080` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Optional OTLP HTTP endpoint; tracing otherwise disabled |

The account can be set in two ways: environment variables, or the settings page
of [mcp-gateway](https://github.com/Olsenius/mcp-gateway). Environment variables
always win and show as locked on the settings page. Settings from the gateway
are stored in `DATA_DIR/state.json` (mode 0600), including the PIN. Login
cookies are held in memory only and lost at restart. Each process serves one
account. Standard `OTEL_*` resource/exporter settings are supported with
HTTP/protobuf. Do not place credentials or personal data in telemetry resource
attributes.

Run `go run .` with the required environment, or use the image
`ghcr.io/olsenius/tibi-mcp:latest`, built from the Dockerfile on every push to
main. Run it behind mcp-gateway: add `tibi=http://tibi:8080` to `UPSTREAMS`, set
`UPSTREAM_TOKEN_TIBI` to the server's `MCP_AUTH_TOKEN`, and give the container a
`/data` volume.

## Gateway endpoints

The server follows mcp-gateway's upstream contract:

| Endpoint | Purpose |
| --- | --- |
| `GET /healthz` | Liveness, no auth |
| `GET /healthz?check=1` | Logs in to Tibi and reports whether a Kindle address is set (never the address) |
| `GET /config` | Settings schema and values; the PIN is never returned |
| `POST /config` | `{"values": {...}, "clear": [...]}`; saves, then reloads without a restart |

Settings: `TIBI_CARD_NUMBER`, `TIBI_PIN` and `MCP_READ_ONLY`. Turning
read-only off adds the write tools at once; turning it on removes them.

## Tools

1. `search_ebooks(query, page)` searches e-books; page is 1-based, default 1.
2. `get_ebook(edition_id)` accepts a search result's `catalog_edition_id` and
   returns actual e-book `edition_id` values and current eligibility.
3. `borrow_ebook(edition_id)` borrows that exact e-book. Already borrowed books
   return `already_borrowed` without another mutation.
4. `send_to_kindle(edition_id)` requests sending an already borrowed, eligible
   e-book to the address configured in Tibi. `send_requested` means Tibi accepted
   the request, not confirmed delivery to the device.
5. `get_epub_link(edition_id)` returns Tibi's direct EPUB link for a borrowed
   e-book, with filename, MIME type and size when known, so another tool can
   fetch the file. The link works without login and contains the borrower
   number: pass it only to the service that should fetch the book.
6. `get_kindle_email()` shows the Kindle or PocketBook address on the profile.
7. `set_kindle_email(kindle_email)` changes only that address. It accepts
   `@kindle.com`, `@free.kindle.com` and `@pbsync.com` addresses, or an empty
   string to remove it, and reads the profile again to confirm the change.

Search hits can reference an audio edition of a work that also has an e-book;
resolve them with `get_ebook` before borrowing. Writes reject non-e-book IDs.
The write tools (`borrow_ebook`, `send_to_kindle`, `set_kindle_email`) are
absent in read-only mode; `get_epub_link` and `get_kindle_email` are read-only. Never automatically retry a
write after a timeout: the first request may have succeeded.

Register the Kindle address under Tibi's profile and approve
`TL-administrator@nb.no` in Amazon's approved senders. Tibi documents a 50 MB
limit; server eligibility is taken from Tibi's current `showKindleButton`.
See https://tibi.no/hjelp/medietyper/e-boker/kindle-pocketbook/. While Tibi's
Kindle sending is out of order, `send_requested` does not lead to delivery.
The Kindle address is the only profile field the server can change, and only
to Kindle or PocketBook domains, so books cannot be sent to arbitrary e-mail
recipients.

## Verification

`go vet ./...` and `go test -race ./...` run without credentials. Workspace users
can run `task test:tibi`. Tests use synthetic HTTP services and in-memory MCP
transports, covering authentication flow, mutation guards, no automatic replay,
redaction, bearer auth and read-only tools. See docs/api.md for protocol evidence.

Current local validation: race tests, go vet, Go build and public live smoke test
passed. Docker image build was blocked by host Docker socket permissions; image
startup and gateway-to-container connectivity have not been exercised.
