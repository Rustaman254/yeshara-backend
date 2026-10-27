// Package cometengine is Yeshara's client for the tokenization engine it's
// a tenant of. Yeshara never touches Stellar (or any chain) directly —
// every mint, transfer, and balance check routes through comet-engine's
// receipt-tokenization API, authenticated as Yeshara's own tenant
// (X-API-Key/X-API-Secret), the same self-service model any tenant uses.
package cometengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	baseURL    string
	apiKey     string
	apiSecret  string
	network    string
	httpClient *http.Client
}

// New builds a client pinned to one engine network ("testnet" or
// "mainnet"), sent as X-Network on every call so Yeshara's offerings,
// mints and balances always land on the network this deployment is
// configured for, never on whatever the engine happens to default to.
func New(baseURL, apiKey, apiSecret, network string) *Client {
	return &Client{
		baseURL:    baseURL,
		apiKey:     apiKey,
		apiSecret:  apiSecret,
		network:    network,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// Network is the engine network this client is pinned to.
func (c *Client) Network() string { return c.network }

// APIError is returned when comet-engine responds with a non-2xx status —
// callers can surface Message directly, it's already meant for an API
// consumer (sanitizeError'd on the comet-engine side).
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("comet-engine: %d: %s", e.StatusCode, e.Message)
}

// PendingApprovalError means comet-engine's maker-checker held the action
// for its approval policy (HTTP 202): nothing happened yet, so callers must
// not treat it as success. It runs once the policy's signers approve.
type PendingApprovalError struct {
	ApprovalID  string
	ActionScope string
	Threshold   int
}

func (e *PendingApprovalError) Error() string {
	return fmt.Sprintf("comet-engine: %s is awaiting %d approval(s) (approval %s)", e.ActionScope, e.Threshold, e.ApprovalID)
}

func (c *Client) do(ctx context.Context, method, path string, body interface{}, out interface{}) error {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("X-API-Secret", c.apiSecret)
	if c.network != "" {
		req.Header.Set("X-Network", c.network)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("comet-engine unreachable: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 300 {
		var errBody struct {
			Error   string `json:"error"`
			Details string `json:"details"`
		}
		msg := string(respBody)
		if json.Unmarshal(respBody, &errBody) == nil && errBody.Error != "" {
			msg = errBody.Error
			if errBody.Details != "" {
				msg += ": " + errBody.Details
			}
		}
		return &APIError{StatusCode: resp.StatusCode, Message: msg}
	}

	if resp.StatusCode == http.StatusAccepted {
		var pending struct {
			Status      string `json:"status"`
			ApprovalID  string `json:"approvalId"`
			ActionScope string `json:"actionScope"`
			Threshold   int    `json:"threshold"`
		}
		if json.Unmarshal(respBody, &pending) == nil && pending.Status == "pending_approval" {
			return &PendingApprovalError{ApprovalID: pending.ApprovalID, ActionScope: pending.ActionScope, Threshold: pending.Threshold}
		}
	}

	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// ── Receipts (offerings) ────────────────────────────────────────────────

type Receipt struct {
	ID                 string                 `json:"id"`
	StandardCode       string                 `json:"standardCode"`
	Symbol             string                 `json:"symbol"`
	Chain              string                 `json:"chain"`
	Identifier         string                 `json:"identifier"`
	Attributes         map[string]interface{} `json:"attributes"`
	Quantity           float64                `json:"quantity"`
	QuantityUnit       string                 `json:"quantityUnit"`
	Decimals           int                    `json:"decimals"`
	RedeemedQuantity   float64                `json:"redeemedQuantity"`
	ClawedBackQuantity float64                `json:"clawedBackQuantity"`
	Network            string                 `json:"network"`
	Shariah            *ShariahProfile        `json:"shariah,omitempty"`
	DepositorUserID    string                 `json:"depositorUserId"`
	Status             string                 `json:"status"`
	MintTxHash         string                 `json:"mintTxHash"`
	// TxURL is comet-engine's ready-to-use block-explorer link for the mint
	// transaction; AssetURL is the link to the asset/token itself (its
	// Stellar credit-line or EVM contract page) — a different explorer page
	// than one specific transaction. Both empty until the receipt is minted.
	TxURL    string `json:"txUrl"`
	AssetURL string `json:"assetUrl"`
}

// Outstanding is the receipt's units still in circulation.
func (r *Receipt) Outstanding() float64 {
	return r.Quantity - r.RedeemedQuantity - r.ClawedBackQuantity
}

// The two engine standards Yeshara deposits under. A Shariah offering
// lives in its own standard, which the engine keeps as a separate pool
// (own journal accounts, certificate required before minting).
const (
	PrimaryStandard = "YESHARA_PRIMARY_OFFERING"
	ShariahStandard = "YESHARA_SHARIAH_OFFERING"
)

// ShariahProfile mirrors comet-engine's shariah.Profile.
type ShariahProfile struct {
	Structure          string                `json:"structure" bson:"structure"`
	Sector             string                `json:"sector" bson:"sector"`
	DebtToAssets       *float64              `json:"debtToAssets,omitempty" bson:"debt_to_assets,omitempty"`
	ImpermissibleRatio *float64              `json:"impermissibleRatio,omitempty" bson:"impermissible_ratio,omitempty"`
	PurificationRatio  float64               `json:"purificationRatio,omitempty" bson:"purification_ratio,omitempty"`
	Certification      *ShariahCertification `json:"certification,omitempty" bson:"certification,omitempty"`
}

// ShariahCertification is the Shariah board's certificate for one offering.
type ShariahCertification struct {
	Board          string    `json:"board" bson:"board"`
	CertificateRef string    `json:"certificateRef" bson:"certificate_ref"`
	Scholars       []string  `json:"scholars,omitempty" bson:"scholars,omitempty"`
	IssuedAt       time.Time `json:"issuedAt" bson:"issued_at"`
	ExpiresAt      time.Time `json:"expiresAt" bson:"expires_at"`
	DocumentURL    string    `json:"documentUrl,omitempty" bson:"document_url,omitempty"`
}

// DepositOffering records a new primary-market offering as a comet-engine
// receipt — under the Shariah standard when shariah is set, otherwise the
// primary one. No units exist on-chain until ApproveOffering verifies it.
func (c *Client) DepositOffering(ctx context.Context, externalUserID, chain string, attributes map[string]interface{}, shariah *ShariahProfile) (*Receipt, error) {
	var out struct {
		ID     string `json:"id"`
		Symbol string `json:"symbol"`
		Status string `json:"status"`
	}
	body := map[string]interface{}{
		"externalUserId": externalUserID,
		"standardCode":   PrimaryStandard,
		"chain":          chain,
		"attributes":     attributes,
	}
	if shariah != nil {
		if err := c.EnsureShariahStandard(ctx); err != nil {
			return nil, err
		}
		body["standardCode"] = ShariahStandard
		body["shariah"] = shariah
	}
	err := c.do(ctx, http.MethodPost, "/api/v1/tokenize/receipt/deposit", body, &out)
	if err != nil {
		return nil, err
	}
	return c.GetReceipt(ctx, out.ID)
}

// ApproveOffering verifies the receipt, minting its full unit supply to
// the depositor's wallet (Yeshara's own treasury identity) in one
// transaction — the primary-market "shares now exist and are available to
// sell" moment.
func (c *Client) ApproveOffering(ctx context.Context, receiptID, verifiedBy string) (txHash, txURL, assetURL string, err error) {
	var out struct {
		TxHash   string `json:"txHash"`
		TxURL    string `json:"txUrl"`
		AssetURL string `json:"assetUrl"`
	}
	err = c.do(ctx, http.MethodPost, "/api/v1/tokenize/receipt/"+receiptID+"/verify", map[string]interface{}{
		"verifiedBy": verifiedBy,
	}, &out)
	return out.TxHash, out.TxURL, out.AssetURL, err
}

// CertifyShariah attaches the board's certificate to a Shariah offering's
// receipt; the engine refuses to mint it without one.
func (c *Client) CertifyShariah(ctx context.Context, receiptID string, cert ShariahCertification) (*Receipt, error) {
	var r Receipt
	if err := c.do(ctx, http.MethodPost, "/api/v1/tokenize/receipt/"+receiptID+"/shariah/certification", cert, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// EnsureShariahStandard creates YESHARA_SHARIAH_OFFERING on the engine if
// it doesn't exist yet, with the same fields as the primary standard so
// both kinds of offering carry identical attributes.
func (c *Client) EnsureShariahStandard(ctx context.Context) error {
	var existing map[string]interface{}
	err := c.do(ctx, http.MethodGet, "/api/v1/tokenize/receipt/standards/"+ShariahStandard, nil, &existing)
	if err == nil {
		return nil
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
		return err
	}
	var primary map[string]interface{}
	if err := c.do(ctx, http.MethodGet, "/api/v1/tokenize/receipt/standards/"+PrimaryStandard, nil, &primary); err != nil {
		return fmt.Errorf("read %s to copy its fields: %w", PrimaryStandard, err)
	}
	body := map[string]interface{}{
		"code":                ShariahStandard,
		"name":                "Yeshara Shariah-compliant offering",
		"description":         "Shariah-compliant primary offerings, kept as a separate pool",
		"symbolPrefix":        primary["symbolPrefix"],
		"quantityUnit":        primary["quantityUnit"],
		"decimals":            primary["decimals"],
		"chain":               primary["chain"],
		"requireVerification": true,
		"fields":              primary["fields"],
		"shariah":             true,
	}
	err = c.do(ctx, http.MethodPost, "/api/v1/tokenize/receipt/standards", body, nil)
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
		return nil // created concurrently
	}
	return err
}

func (c *Client) RejectOffering(ctx context.Context, receiptID, reason string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/tokenize/receipt/"+receiptID+"/reject", map[string]interface{}{
		"reason": reason,
	}, nil)
}

func (c *Client) GetReceipt(ctx context.Context, receiptID string) (*Receipt, error) {
	var r Receipt
	if err := c.do(ctx, http.MethodGet, "/api/v1/tokenize/receipt/"+receiptID, nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (c *Client) ListReceipts(ctx context.Context, status string) ([]Receipt, error) {
	path := "/api/v1/tokenize/receipt?standardCode=" + PrimaryStandard
	if status != "" {
		path += "&status=" + status
	}
	var out struct {
		Receipts []Receipt `json:"receipts"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Receipts, nil
}

// ── Wallets & transfers ─────────────────────────────────────────────────

// EnsureWallet creates (or fetches, idempotently) an investor's custodial
// Stellar wallet under the Yeshara tenant.
func (c *Client) EnsureWallet(ctx context.Context, externalUserID, chain string) (address string, err error) {
	var out struct {
		Address string `json:"address"`
	}
	err = c.do(ctx, http.MethodPost, "/api/v1/wallets/create?chain="+chain, map[string]interface{}{
		"externalUserId": externalUserID,
	}, &out)
	return out.Address, err
}

// TransferUnits moves `amountBase` base units of `symbol` from Yeshara's
// treasury identity to an investor — the on-chain moment an investment is
// actually fulfilled, called only after payment is confirmed.
func (c *Client) TransferUnits(ctx context.Context, symbol, chain, fromExternalUserID, toAddress string, amountBase int64) (txHash string, err error) {
	var out struct {
		TxHash string `json:"txHash"`
	}
	err = c.do(ctx, http.MethodPost, "/api/v1/assets/"+symbol+"/send?chain="+chain, map[string]interface{}{
		"externalUserId": fromExternalUserID,
		"to":             toAddress,
		"amountBase":     amountBase,
	}, &out)
	return out.TxHash, err
}

// AssetBalance returns an investor's holding of `symbol` in human-readable
// units (already divided by decimals).
func (c *Client) AssetBalance(ctx context.Context, symbol, chain, externalUserID string) (float64, error) {
	var out struct {
		HumanAmount float64 `json:"humanAmount"`
	}
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/v1/assets/%s/balance?chain=%s&externalUserId=%s", symbol, chain, externalUserID), nil, &out)
	return out.HumanAmount, err
}

// ── Yield distribution & redemption ─────────────────────────────────────

// Payout is one recipient's share in a Distribute call.
type Payout struct {
	ExternalUserID string  `json:"externalUserId"`
	Amount         float64 `json:"amount"`
}

// PayoutResult is one payout's outcome, echoed back per-entry so a partial
// batch failure doesn't need retrying wholesale.
type PayoutResult struct {
	ExternalUserID string  `json:"externalUserId"`
	Amount         float64 `json:"amount"`
	TxHash         string  `json:"txHash"`
	Error          string  `json:"error"`
}

type DistributeResult struct {
	Status           string         `json:"status"`
	ReceiptID        string         `json:"receiptId"`
	PayoutSymbol     string         `json:"payoutSymbol"`
	TotalDistributed float64        `json:"totalDistributed"`
	Results          []PayoutResult `json:"results"`
	// Set for Shariah receipts: what the issuer owes charity (purification).
	PurificationAmount float64 `json:"purificationAmount,omitempty"`
}

// Distribute pays a batch of holders from a receipt's income — rental,
// sale-proceeds, whatever the tenant is distributing. payoutSymbol must
// already be a registered asset on the receipt's chain; comet-engine mints
// it fresh to each recipient, no pre-funded balance required on Yeshara's
// side. Per-payout failures are isolated in Results, not fatal to the batch.
//
// distributionType says what the payout is (rental, profit_share,
// sale_proceeds, capital_return); the engine requires it for Shariah
// receipts and refuses anything that is a fixed or guaranteed return.
func (c *Client) Distribute(ctx context.Context, receiptID, payoutSymbol, externalID, distributionType string, payouts []Payout) (*DistributeResult, error) {
	var out DistributeResult
	err := c.do(ctx, http.MethodPost, "/api/v1/tokenize/receipt/"+receiptID+"/distribute", map[string]interface{}{
		"payoutSymbol":     payoutSymbol,
		"payouts":          payouts,
		"externalId":       externalID,
		"distributionType": distributionType,
	}, &out)
	return &out, err
}

type RedeemResult struct {
	Status         string  `json:"status"`
	ID             string  `json:"id"`
	Symbol         string  `json:"symbol"`
	RedeemedAmount float64 `json:"redeemedAmount"`
	QuantityUnit   string  `json:"quantityUnit"`
	Outstanding    float64 `json:"outstanding"`
	ReceiptStatus  string  `json:"receiptStatus"`
	TxHash         string  `json:"txHash"`
}

// Redeem burns `amount` of a receipt's tokens out of externalUserID's own
// custodial wallet and records the redemption against the receipt's
// outstanding quantity — the on-chain "claim the underlying asset back"
// moment. It does not move any other asset/cash to the user; settlement of
// the actual payout is a separate step (see B2CTransfer / Distribute).
func (c *Client) Redeem(ctx context.Context, receiptID, externalUserID, externalID string, amount float64) (*RedeemResult, error) {
	var out RedeemResult
	err := c.do(ctx, http.MethodPost, "/api/v1/tokenize/receipt/"+receiptID+"/redeem", map[string]interface{}{
		"externalUserId": externalUserID,
		"amount":         amount,
		"externalId":     externalID,
	}, &out)
	return &out, err
}

type B2CResult struct {
	Status        string `json:"status"`
	SecureID      string `json:"secureId"`
	TransactionID string `json:"transactionId"`
}

// B2CTransfer sends amountKES to phone via comet-engine's M-Pesa/Airtel
// disbursement rail. It debits the tenant's comet-engine KES treasury
// ledger balance (see TreasuryBalance) — the call fails outright if that
// balance is short, so check it first rather than surfacing a confusing
// mid-payout error to an admin.
func (c *Client) B2CTransfer(ctx context.Context, externalUserID, phone, mobileMoneySP, externalID string, amountKES float64) (*B2CResult, error) {
	var out B2CResult
	err := c.do(ctx, http.MethodPost, "/api/v1/admin/topup/transfer", map[string]interface{}{
		"externalUserId": externalUserID,
		"phone":          phone,
		"amount":         amountKES,
		"mobileMoneySP":  mobileMoneySP,
		"externalId":     externalID,
	}, &out)
	return &out, err
}

// TreasuryBalance returns the tenant's current comet-engine KES treasury
// ledger balance — what B2CTransfer can actually pay out before it starts
// rejecting individual transfers. Yeshara's investment payments settle
// through Mamlaka's PSP directly and never credit this ledger, so today
// this is 0 unless something else (e.g. an admin topping it up via
// comet-engine's own STK-push-to-self) has funded it.
func (c *Client) TreasuryBalance(ctx context.Context) (float64, error) {
	var out struct {
		Balances []struct {
			Account string  `json:"account"`
			Balance float64 `json:"balance"`
		} `json:"balances"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/journal/balances", nil, &out); err != nil {
		return 0, err
	}
	for _, b := range out.Balances {
		if strings.HasSuffix(b.Account, ":treasury:KES") {
			return b.Balance, nil
		}
	}
	return 0, nil
}
