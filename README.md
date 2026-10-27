# yeshara-backend

The Yeshara marketplace API (Go/Gin, MongoDB, default port `:8200`). It is a tenant of comet-engine: every offering, mint, transfer, distribution and burn goes through the engine's receipt-tokenization API, and Yeshara never talks to a chain directly.

```bash
cp .env.example .env   # fill it in
go run ./cmd/server
go test ./...          # add TEST_MONGO_URI=... to run the Mongo-backed tests
```

## Configuration

| Variable | Notes |
|---|---|
| `COMET_ENGINE_BASE_URL`, `COMET_API_KEY`, `COMET_API_SECRET` | Yeshara's engine tenant credentials. Required. |
| `COMET_ENGINE_NETWORK` | `testnet` (default) or `mainnet`; aliases such as `prod` and `sandbox` are accepted. Sent as `X-Network` on every engine call. |
| `PAYMENTS_BASE_URL`, `PAYMENTS_MERCHANT_ID`, `PAYMENTS_CALLBACK_URL`, `PAYMENTS_CALLBACK_SECRET` | Mamlaka PSP (M-Pesa/Airtel). The callback secret is required. |
| `ADMIN_TOKEN`, `ADMIN_EMAIL`, `ADMIN_PASSWORD_HASH` | Admin API and login. |
| `TREASURY_USER_ID` | The custodial identity that holds unsold units (default `yeshara-treasury`). |

**Environments must match.** The server refuses to start with `COMET_ENGINE_NETWORK=mainnet` and the sandbox payments URL, because that would mint real units against fake payments. It also refuses `testnet` with the live payments URL, because investors would pay real money for test units.

## Offering lifecycle

1. **Create** (`POST /api/v1/admin/offerings`). The offering is deposited on the engine as a receipt under `YESHARA_PRIMARY_OFFERING`, or under `YESHARA_SHARIAH_OFFERING` for a Shariah offering.
2. **Review.** Manager A, then Manager B, then Trustee A (staff routes under `/api/v1/staff`).
3. **Tokenize.** Trustee B mints the full supply to the treasury and reserves 20% for the owner. If the engine's maker-checker holds the mint, staff get `202 pending_approval`; tokenize again once the engine signers approve.
4. **Live.** Investors buy units (M-Pesa or on-chain), and receive distributions and redemptions.
5. **Delist / relist** (`/delist`, `/relist`) hides the offering from the marketplace and back. It changes nothing on-chain.
6. **Remove** (`POST /api/v1/admin/offerings/:id/remove`, body `{removedBy, reason}`) retires a live or delisted offering. It answers 409 while investors still hold units (receipt outstanding minus the treasury and owner-reserve balances) or while any payment is pending. Otherwise the remaining treasury and owner-reserve units are burned on the engine, and the offering becomes `removed`.

A pending offering is stopped with the staff reject action instead.

## Shariah-compliant offerings (opt-in)

An offering is Shariah-compliant only when the admin chooses it at creation, by sending a `shariah` block (`structure`, `sector`, and optionally `debtToAssets` and `purificationRatio`). Offerings without it are conventional.

- The engine screens the profile when the offering is created (422 on failure) and keeps Shariah offerings in their own pool. The pool's standard is created on first use by copying the primary standard's fields.
- The Shariah board's certificate must be recorded before Trustee B can tokenize: `POST /api/v1/admin/offerings/:id/shariah-certification` with `{board, certificateRef, issuedAt, expiresAt}`.
- Distributions take a `distributionType`, defaulting to `rental`. Shariah offerings accept only `rental`, `profit_share`, `sale_proceeds` and `capital_return`. Redemption payouts are sent as `capital_return`.
- `GET /api/v1/offerings?shariah=true` lists only Shariah offerings.

The engine-side rules are in comet-engine's `docs/TOKENIZATION_CONTROLS.md`.

## Engine compatibility

- A `202 pending_approval` from the engine is returned as `cometengine.PendingApprovalError` and never treated as success.
- Deploy comet-engine before this service. An older engine ignores the Shariah fields.
