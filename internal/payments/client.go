// Package payments is Yeshara's client for Mamlaka's own PSP
// (payments.mamlakapsp.com) — the mobile-money rail investors pay KES
// through. STK push collection is a fire-and-forget request: a 200 here
// only means the prompt was sent to the investor's phone, not that they've
// paid. The actual outcome (COMPLETE/FAILED) arrives later on the callback
// URL given at initiation — see api.mobileMoneyCallback.
package payments

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Provider string

const (
	ProviderMPesa  Provider = "mpesa"
	ProviderAirtel Provider = "airtel"
)

// mobileMoneySP is the provider code Mamlaka's API expects.
func (p Provider) mobileMoneySP() (string, error) {
	switch p {
	case ProviderMPesa:
		return "M-Pesa", nil
	case ProviderAirtel:
		return "AIRTEL", nil
	default:
		return "", fmt.Errorf("payments: unknown provider %q", p)
	}
}

type Client struct {
	baseURL    string
	merchantID string
	httpClient *http.Client
}

func New(baseURL, merchantID string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		merchantID: merchantID,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// APIError is returned when Mamlaka responds with a non-2xx status.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("payments: %d: %s", e.StatusCode, e.Message)
}

type InitiateResult struct {
	SecureID string
	Message  string
}

// InitiateSTK sends an STK push prompt to the investor's phone for
// amountKES. displayName is what the investor sees on the prompt.
// externalID is Yeshara's own reference (the investment's PaymentRef) —
// the callback echoes it back so mobileMoneyCallback can find the order.
func (c *Client) InitiateSTK(provider Provider, displayName, phone string, amountKES float64, externalID, callbackURL string) (*InitiateResult, error) {
	sp, err := provider.mobileMoneySP()
	if err != nil {
		return nil, err
	}

	payload := map[string]interface{}{
		"impalaMerchantId": c.merchantID,
		"displayName":      displayName,
		"currency":         "KES",
		"amount":           amountKES,
		"payerPhone":       phone,
		"mobileMoneySP":    sp,
		"externalId":       externalID,
		"callbackUrl":      callbackURL,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/mobile/initiate", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("payments API unreachable: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var out struct {
		Message  string `json:"message"`
		SecureID string `json:"secureId"`
	}
	if resp.StatusCode >= 300 {
		msg := string(respBody)
		var errBody struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(respBody, &errBody) == nil {
			if errBody.Message != "" {
				msg = errBody.Message
			} else if errBody.Error != "" {
				msg = errBody.Error
			}
		}
		return nil, &APIError{StatusCode: resp.StatusCode, Message: msg}
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &InitiateResult{SecureID: out.SecureID, Message: out.Message}, nil
}

// CallbackPayload is what Mamlaka posts back to PaymentsCallbackURL once an
// STK push resolves.
type CallbackPayload struct {
	SecureID          string  `json:"secureId"`
	ExternalID        string  `json:"externalId"`
	TransactionStatus string  `json:"transactionStatus"` // COMPLETE | FAILED | PENDING
	TransactionReport string  `json:"transactionReport"`
	NetAmount         float64 `json:"netAmount"`
	Currency          string  `json:"currency"`
	Reference         string  `json:"reference"` // telco receipt number (e.g. M-Pesa receipt), set on success
}

// VerifySignature checks the X-Mamlaka-Signature header
// ("sha256=<hex hmac>") against rawBody using constant-time comparison.
// Fails closed: an empty secret rejects every request rather than skipping
// verification, since the caller (mobileMoneyCallback) is a public,
// unauthenticated endpoint that flips a payment to COMPLETE and mints
// on-chain — the signature is the only thing standing between "Mamlaka
// confirmed this payment" and "anyone on the internet POSTed a JSON body."
// config.Load() also requires PaymentsCallbackSecret to be set at startup,
// but this check exists independently in case that's ever bypassed.
func VerifySignature(secret string, rawBody []byte, header string) bool {
	if secret == "" {
		return false
	}
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(rawBody)
	return hmac.Equal(sig, mac.Sum(nil))
}
