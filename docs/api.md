# Tibi protocol evidence

Inspected 2026-11-02. These are private website endpoints, not a promised public
API. Requests use the Tibi website session cookie; no access token is exposed
through MCP. Query parameters are URL-encoded.

## Sources and confidence

- Public bundle: https://tibi.no/boker/_prod/scripts/nlb.min.js.
  `yc` implements `search`, `getBookPageByEdition`, `addBookToShelf` and
  `sendToKindle`; `gc.url` prepends `/boker`.
- REA static analysis evidence:
  `ev_0c4feaa9a36a7c8eb9df46418e10b3c0a6d4a4ced433ed6562ace7d80b6f9be6`;
  summary `ev_f8b3db681b5791774064f74e3e77fad7deb3b23595294d15c62966ca3cc9b884`.
  Directory digest `9103cd140262f36c0435e21bc3826e6fccd3319974cd78d12017edc7429dfab9`.
  One 832033-byte JavaScript file parsed, no parse failures. Semantic analysis
  reached its 100000-node ceiling; it is partial and does not prove execution.
  Endpoint and payload conclusions below additionally use focused source reads.
- Public GET search and book-detail responses were observed live without an
  account. Search results contain `presentationCards`, `paging`, `result`.
- Tibi's help pages document borrowing before Kindle sending, profile setup and
  a 50 MB maximum: https://tibi.no/hjelp/medietyper/e-boker/ and
  https://tibi.no/hjelp/medietyper/e-boker/kindle-pocketbook/.

## Catalog and actions

| Method | Route under /boker | Parameters | Evidence |
| --- | --- | --- | --- |
| GET | /search/search | query, Format=EBook, page (1-based) | Client and public live response |
| GET | /book/GetBookPageByEdition | editionId | Client and public live response |
| POST | /patron/addBookToShelf | editionId | Static client only; boolean result |
| GET | /patron/SendToKindle | editionId | Static client only; boolean result |

Search's format facet returns `EBook`. Presentation cards can have an audio
edition in `bookUrl` even when filtered to e-books; never borrow that identifier
without resolving the work's `editions`. Each edition has `format.type`,
`editionId`, `isAvailable`, `isCopiesAvailableForLoan`, `showLoanBookButton`,
`showKindleButton`, and `editionOnPatronShelf.isOnPatronShelf`. The observed client
format enum is 4=Ebook, 6=Epub. Only these types are allowed for mutation.

Only selected bibliographic data and action booleans leave the client. It does
not expose raw patron objects, player/download URLs, cookie values or upstream
error bodies. A true Kindle response means accepted, not proof of delivery.
Neither mutation is automatically retried. Despite its GET method, SendToKindle
is a write and disabled in read-only mode. An HTTP redirect is never followed
for a catalog/API call or mutation.

## Authentication

Always start a fresh GET `/boker/Auth/Login?returnUrl=%2Fboker%2F`, retaining
correlation cookies in the in-memory cookie jar. Do not replay a supplied login
URL: state and nonce belong to a particular login attempt.

The redirect reaches auth.nlb.no. Its page contains `data-initial-state` with a
CSRF token and `Selector` component. The public auth bundle
`/web-app/generated/index-BvHjBFyh.js` supplies the form submission protocol:
POST to the current pathname (without its OIDC query), form-encoded, with
`Accept: application/json` and `X-Csrf-Token`.

1. Submit `component=Selector`, `selectorId=props.id`,
   `selectedAuthenticatorId=Lnr_PIN`.
2. Observed live response: `DynamicInputs` with username/Text and
   password/Password. Submit `component=DynamicInputs`, username=borrower number,
   password=PIN and `action=submit`, once only.
3. The public auth client implements `Post` with props.url/form and `Redirect`
   with props.url. Follow only the expected Tibi/auth origins; a Post callback
   must target exactly `https://tibi.no/boker/auth/callback` and contain code/state.
4. POST the callback once without following its redirect. Observed live
   (2026-11-02): the credential POST redirects back to the auth login page,
   which returns `Post` to the callback. The callback sets the chunked
   `.AspNetCore.Cookies` session and redirects to `/boker/Auth/Login`. That
   endpoint always starts a new OIDC challenge, so following it loops into a
   second sign-in. Accept a 200 or a same-origin 302/303 from the callback, then
   independently fetch the book details and require `isAuthenticated=true`
   before any write.

All steps were observed live with a real account on 2026-11-02, using only
sanitized logs (methods, hosts, paths, status, component and cookie names).
An authenticated book lookup returned `isAuthenticated=true`. Borrowing and
Kindle delivery remain unverified against a real account. CAPTCHA or
unexpected input fields
fail closed and require signing in through the website.

## Remaining verification

Run `go test -tags live -run TestLivePublicCatalog -v` for a public, read-only
smoke test. It never loads credentials. To verify the remaining flow, the account
owner must configure credentials privately, then check `get_ebook` reports
`authenticated=true`. Borrow and send only a specifically requested title and
confirm actual Kindle receipt. No production deployment or release is implied.
