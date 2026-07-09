---
name: sub2api-admin
description: Manage Sub2API admin APIs for accounts, redeem codes, groups, proxies, error passthrough rules, TLS fingerprint profiles, imports, exports, batch updates, and raw administrator API calls. Use when the user mentions Sub2API, admin API keys, account management, redeem code management, recharge codes, invitation codes, bulk account import/export, keeping or deleting accounts, refreshing accounts, clearing errors, CRS sync, or managing Sub2API backend settings through the admin API.
---

# Sub2API Admin

Use the bundled CLI instead of ad hoc `curl`. Run examples from this skill directory.

```bash
export SUB2API_BASE_URL='https://your-sub2api-host'
export SUB2API_ADMIN_API_KEY='<admin api key>'
# Or, when the deployment uses admin JWT login instead of an admin API key:
# export SUB2API_JWT='<admin access_token>'
node scripts/sub2api-admin.js accounts list
```

For all commands and payload examples, read [references/admin-cli.md](references/admin-cli.md).

## Workflow

1. Reuse `SUB2API_BASE_URL` and either `SUB2API_ADMIN_API_KEY` or `SUB2API_JWT` from the environment.
2. Run read-only commands first: `accounts list`, `accounts get <id>`, `groups all`, or `proxies all`.
3. Before destructive or bulk writes, print the target account names and IDs.
4. Execute the write command only after the target set is clear.
5. Run a follow-up read command to verify the result.

## Common Commands

```bash
node scripts/sub2api-admin.js accounts list --page-size 20
node scripts/sub2api-admin.js accounts get 40
node scripts/sub2api-admin.js accounts usage 40
node scripts/sub2api-admin.js accounts set-schedulable 40 true
node scripts/sub2api-admin.js accounts bulk-update --ids 40,39 --json '{"concurrency":10}'
node scripts/sub2api-admin.js accounts bulk-update --ids 40 --json '{"extra":{"equivalent_cache_billing_enabled":true}}'
node scripts/sub2api-admin.js redeem-codes list --page-size 20
node scripts/sub2api-admin.js redeem-codes generate --json '{"count":1,"type":"balance","value":10}' --idempotency-key redeem-$(date +%s)
node scripts/sub2api-admin.js redeem-codes create-and-redeem --json '{"code":"order_123","type":"balance","value":10,"user_id":123}' --idempotency-key order-123
node scripts/sub2api-admin.js error-rules list
node scripts/sub2api-admin.js tls-profiles list
```

## Kiro Equivalent Cache Billing

Use this only for self-owned Kiro accounts in new-machine or warmup environments. Production entry machines are read-only for this workflow; do not stop, restart, or mutate them.

1. Verify the target first: `accounts list --search <name>` and `accounts get <id>`.
2. Enable with key-level merge:
   `accounts bulk-update --ids <id> --json '{"extra":{"equivalent_cache_billing_enabled":true,"equivalent_cache_billing_loss_factor":1.08,"equivalent_cache_billing_input_share":0.20,"equivalent_cache_billing_cache_read_share":0.75,"equivalent_cache_billing_cache_creation_share":0.05}}'`
3. Disable with:
   `accounts bulk-update --ids <id> --json '{"extra":{"equivalent_cache_billing_enabled":false}}'`
4. Re-run `accounts get <id>` and verify usage logs/front-end cache display after one or two test requests.

Do not enable it for external Kiro upstream accounts. Do not route `cloudflare-temp-email` through the relay network. Prefer `bulk-update` for extra-only changes because it merges keys; `accounts update` replaces the whole `extra` object.

Full runbook: [docs/EQUIVALENT_CACHE_BILLING_CN.md](../../docs/EQUIVALENT_CACHE_BILLING_CN.md).

## Safety Notes

- Authentication uses `x-api-key` from `SUB2API_ADMIN_API_KEY` first, then falls back to `Authorization: Bearer <jwt>` from `SUB2API_JWT`.
- If the API returns `INVALID_ADMIN_KEY`, ask the user to regenerate the admin API key. If using JWT, log in as an admin user and copy the `access_token` from `POST /api/v1/auth/login`.
- `accounts export` includes credentials and tokens. Prefer `--file` and avoid printing exports in chat.
- Use `accounts bulk-update --json '{"extra":{...}}'` for extra-only account toggles. Single-account `accounts update --json '{"extra":{...}}'` replaces the entire extra object.
- Redeem code create/redeem commands should use `--idempotency-key` for payment or recharge workflows.
- For uncertain or newly added backend APIs, use `api <METHOD> <admin-path>` after a read-only check.
