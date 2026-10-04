# proxyctl

Build with `go build -o proxyctl ./cmd/proxyctl`. All output is JSON, including
errors. Exit status zero means the management operation succeeded; nonzero
means the command was rejected or failed.

Configure the private server origin and a separate management credential:

```bash
export CLIPROXY_URL=http://levi-thinkcentre-m700.dinosaur-dojo.ts.net:8317
export CLIPROXY_MANAGEMENT_KEY_FILE=/protected/path/management-key
./proxyctl accounts
./proxyctl explain
./proxyctl refresh
./proxyctl refresh CREDENTIAL_ID
./proxyctl resets list
./proxyctl resets redeem CREDENTIAL_ID CREDIT_ID
./proxyctl resets schedule CREDENTIAL_ID CREDIT_ID 2026-10-25T01:30:00+02:00
./proxyctl resets cancel SCHEDULE_ID
```

`CLIPROXY_URL` must contain only an HTTP or HTTPS origin, without an inference
or management path. Use the actual configured private listener port. The
management key file should be readable only by the operator. Alternatively,
set `CLIPROXY_MANAGEMENT_KEY` through your protected environment. This command
never uses an inference API key and has no command-line key flag. Redirects
are rejected to prevent credential forwarding or unexpected write replay.

Redemption and scheduling require both the credential mapping and the opaque
credit ID from the account inventory. There is no implicit default account or
credit. Schedule timestamps must include `Z` or an explicit UTC offset; a
schedule preserves that instant across daylight-saving changes. It cannot
extend the provider's credit expiry.

`resets list` returns operation history and local schedules. The credit
inventory, completeness, provider expiry, and account ownership are returned
by `accounts`. A successful redemption response still requires the operation
state and observed provider evidence to establish that allowance recovered.
Use the maintained `/account-policy.html` page for settings and diagnostics.

Common error codes include `invalid_arguments`, `invalid_configuration`,
`connection_error`, `unauthorized`, `unavailable`, `invalid_settings`,
`unknown_account`, `unknown_credit`, `conflict`, `read_only`, `persistence`,
`stale_evidence`, and `provider_error`. Unexpected error bodies are withheld.
The connection error points to tailnet and listener checks; provider errors
are reported separately by the authenticated management API.
