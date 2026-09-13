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
	httpClient *http.Client
}

func New(baseURL, apiKey, apiSecret string) *Client {
	return &Client{
		baseURL:    baseURL,
		apiKey:     apiKey,
		apiSecret:  apiSecret,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

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

	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// ── Receipts (offerings) ────────────────────────────────────────────────

type Receipt struct {
	ID               string                 `json:"id"`
	StandardCode     string                 `json:"standardCode"`
	Symbol           string                 `json:"symbol"`
	Chain            string                 `json:"chain"`
	Identifier       string                 `json:"identifier"`
	Attributes       map[string]interface{} `json:"attributes"`
	Quantity         float64                `json:"quantity"`
	QuantityUnit     string                 `json:"quantityUnit"`
	Decimals         int                    `json:"decimals"`
	RedeemedQuantity float64                `json:"redeemedQuantity"`
	DepositorUserID  string                 `json:"depositorUserId"`
	Status           string                 `json:"status"`
	MintTxHash       string                 `json:"mintTxHash"`
}

// DepositOffering records a new primary-market offering as a comet-engine
// receipt under the YESHARA_PRIMARY_OFFERING standard — no units exist
// on-chain until ApproveOffering verifies it.
func (c *Client) DepositOffering(ctx context.Context, externalUserID, chain string, attributes map[string]interface{}) (*Receipt, error) {
	var out struct {
		ID     string `json:"id"`
		Symbol string `json:"symbol"`
		Status string `json:"status"`
	}
	err := c.do(ctx, http.MethodPost, "/api/v1/tokenize/receipt/deposit", map[string]interface{}{
		"externalUserId": externalUserID,
		"standardCode":   "YESHARA_PRIMARY_OFFERING",
		"chain":          chain,
		"attributes":     attributes,
	}, &out)
	if err != nil {
		return nil, err
	}
	return c.GetReceipt(ctx, out.ID)
}

// ApproveOffering verifies the receipt, minting its full unit supply to
// the depositor's wallet (Yeshara's own treasury identity) in one
// transaction — the primary-market "shares now exist and are available to
// sell" moment.
func (c *Client) ApproveOffering(ctx context.Context, receiptID, verifiedBy string) (txHash string, err error) {
	var out struct {
		TxHash string `json:"txHash"`
	}
	err = c.do(ctx, http.MethodPost, "/api/v1/tokenize/receipt/"+receiptID+"/verify", map[string]interface{}{
		"verifiedBy": verifiedBy,
	}, &out)
	return out.TxHash, err
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
	path := "/api/v1/tokenize/receipt?standardCode=YESHARA_PRIMARY_OFFERING"
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
}

// Distribute pays a batch of holders from a receipt's income — rental,
// sale-proceeds, whatever the tenant is distributing. payoutSymbol must
// already be a registered asset on the receipt's chain; comet-engine mints
// it fresh to each recipient, no pre-funded balance required on Yeshara's
// side. Per-payout failures are isolated in Results, not fatal to the batch.
func (c *Client) Distribute(ctx context.Context, receiptID, payoutSymbol, externalID string, payouts []Payout) (*DistributeResult, error) {
	var out DistributeResult
	err := c.do(ctx, http.MethodPost, "/api/v1/tokenize/receipt/"+receiptID+"/distribute", map[string]interface{}{
		"payoutSymbol": payoutSymbol,
		"payouts":      payouts,
		"externalId":   externalID,
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
