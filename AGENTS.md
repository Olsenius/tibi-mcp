# Working in tibi-mcp

- Read the workspace AGENTS.md when working in mcp-workspace.
- Go, single main package. Run gofmt, go vet ./... and go test -race ./....
- Use synthetic account data. Never print credentials, cookies, login state,
  callback codes, private profile fields or upstream response bodies in errors.
- Public read-only catalog checks are permitted. Live borrowing and Kindle
  delivery require a specifically requested book and account-owner instruction.
- Derive endpoint changes from Tibi's public client; record evidence and unknowns
  in docs/api.md. Do not invent endpoints or claim synthetic tests prove live login.
- MCP is stateless; authentication cookies live only in memory. Do not retry a
  mutation automatically, including the GET endpoint used for Kindle sending.
- Spans include fixed routes/status only, never IDs, query strings or payloads.
