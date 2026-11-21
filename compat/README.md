# Compatibility proof

A separate module so the parent `go.mod` never depends on `agent-pro`. It proves
the contract from both ends:

- `agent-pro`'s independent DSH reader lists, inspects and folds sessions this
  backend writes.
- `agent-pro`'s own `web.Client` creates, drafts, selects a model on, and queries
  a session served by this backend over HTTP.

Both tests need `external/agent-pro`:

```sh
wrk --bring agent-pro
(cd compat && go test ./...)
```

Without that worktree the module cannot resolve its `replace` and the tests are
expected to fail at setup rather than silently pass.
