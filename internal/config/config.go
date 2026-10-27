// Package config loads yeshara-backend's environment configuration.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Port string

	MongoURI string
	MongoDB  string

	// CometEngine is the tokenization engine this backend is a tenant of —
	// every offering, mint, and transfer routes through it. Yeshara never
	// talks to a blockchain directly; comet-engine already owns that.
	CometEngineBaseURL string
	CometAPIKey        string
	CometAPISecret     string
	// CometEngineNetwork is the engine network ("testnet" | "mainnet") every
	// engine call is pinned to via X-Network. It must agree with the
	// payments environment: testnet with the sandbox PSP, mainnet with live.
	CometEngineNetwork string

	// TreasuryUserID is the externalUserId comet-engine mints an approved
	// offering's full unit supply to — the "shares available for primary
	// sale" pool an investment transfers units out of. Not a real bank
	// treasury; just the custodial wallet identity Yeshara itself holds on
	// its own tenant.
	TreasuryUserID string

	// AdminToken gates the issuer/admin API surface — a deliberately simple
	// shared secret for this MVP, not a per-user admin role system. Fine
	// for one issuer operating its own marketplace.
	AdminToken string

	// AdminEmail/AdminPasswordHash gate POST /api/v1/admin/login, which on
	// success hands back AdminToken — this is just a friendlier login form
	// in front of the same shared-secret gate, not real per-user admin
	// accounts.
	AdminEmail        string
	AdminPasswordHash string

	// Payments is Mamlaka's own PSP (payments.mamlakapsp.com) — Yeshara's
	// mobile-money (M-Pesa/Airtel) collection rail for KES purchases. STK
	// push (POST /mobile/initiate) is authenticated by merchant ID in the
	// request body, not a bearer token, so only these three are needed.
	PaymentsBaseURL        string
	PaymentsMerchantID     string
	PaymentsCallbackURL    string
	PaymentsCallbackSecret string // required — verifies X-Mamlaka-Signature on every inbound callback (see payments.VerifySignature, which fails closed if this is ever empty)

	// USDKESRate is a static, manually-updated conversion rate used only to
	// quote KES amounts against offerings priced in USD — Yeshara has no
	// live FX feed. Not used for settlement: the STK charge amount is always
	// exactly what the investor sees on the KES input.
	USDKESRate float64
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func Load() (*Config, error) {
	cfg := &Config{
		Port:               getEnv("PORT", "8200"),
		MongoURI:           getEnv("MONGO_URI", "mongodb://localhost:27017"),
		MongoDB:            getEnv("MONGO_DB", "yeshara"),
		CometEngineBaseURL: getEnv("COMET_ENGINE_BASE_URL", "http://localhost:8100"),
		CometAPIKey:        os.Getenv("COMET_API_KEY"),
		CometAPISecret:     os.Getenv("COMET_API_SECRET"),
		CometEngineNetwork: getEnv("COMET_ENGINE_NETWORK", NetworkTestnet),
		TreasuryUserID:     getEnv("TREASURY_USER_ID", "yeshara-treasury"),
		AdminToken:         os.Getenv("ADMIN_TOKEN"),
		AdminEmail:         os.Getenv("ADMIN_EMAIL"),
		AdminPasswordHash:  os.Getenv("ADMIN_PASSWORD_HASH"),

		PaymentsBaseURL:        getEnv("PAYMENTS_BASE_URL", "https://sandbox.payments.mamlakapsp.com/api/v1"),
		PaymentsMerchantID:     os.Getenv("PAYMENTS_MERCHANT_ID"),
		PaymentsCallbackURL:    os.Getenv("PAYMENTS_CALLBACK_URL"),
		PaymentsCallbackSecret: os.Getenv("PAYMENTS_CALLBACK_SECRET"),
		USDKESRate:             129.0,
	}
	if cfg.CometAPIKey == "" || cfg.CometAPISecret == "" {
		return nil, fmt.Errorf("COMET_API_KEY and COMET_API_SECRET are required (create a comet-engine tenant for Yeshara first)")
	}
	if cfg.AdminToken == "" {
		return nil, fmt.Errorf("ADMIN_TOKEN is required (gates the issuer/admin API)")
	}
	if cfg.AdminEmail == "" || cfg.AdminPasswordHash == "" {
		return nil, fmt.Errorf("ADMIN_EMAIL and ADMIN_PASSWORD_HASH are required (gates POST /api/v1/admin/login)")
	}
	if cfg.PaymentsMerchantID == "" || cfg.PaymentsCallbackURL == "" {
		return nil, fmt.Errorf("PAYMENTS_MERCHANT_ID and PAYMENTS_CALLBACK_URL are required (gates M-Pesa/Airtel checkout)")
	}
	if cfg.PaymentsCallbackSecret == "" {
		// Without this, VerifySignature would have nothing to check the
		// inbound callback against — and POST /payments/mobile-money/callback
		// is unauthenticated by design (Mamlaka can't hold an investor
		// session), so an empty secret here means anyone who can guess or
		// intercept a PaymentRef can POST a forged "COMPLETE" callback and
		// have units minted to them for free. Must be set in every
		// environment, not just recommended.
		return nil, fmt.Errorf("PAYMENTS_CALLBACK_SECRET is required (verifies inbound mobile-money callbacks — without it, anyone can forge a payment confirmation)")
	}
	network, ok := NormalizeNetwork(cfg.CometEngineNetwork)
	if !ok {
		return nil, fmt.Errorf("COMET_ENGINE_NETWORK must be testnet or mainnet, got %q", cfg.CometEngineNetwork)
	}
	cfg.CometEngineNetwork = network
	if err := checkEnvironmentsMatch(network, cfg.PaymentsBaseURL); err != nil {
		return nil, err
	}
	if v := os.Getenv("USD_KES_RATE"); v != "" {
		rate, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, fmt.Errorf("USD_KES_RATE must be a number: %w", err)
		}
		cfg.USDKESRate = rate
	}
	return cfg, nil
}

// Engine networks, as comet-engine names them.
const (
	NetworkTestnet = "testnet"
	NetworkMainnet = "mainnet"
)

// NormalizeNetwork accepts the spellings comet-engine accepts (test,
// sandbox, prod, production, live, ...) and returns testnet or mainnet.
func NormalizeNetwork(v string) (string, bool) {
	switch strings.ToLower(strings.ReplaceAll(strings.TrimSpace(v), " ", "")) {
	case "testnet", "test", "sandbox", "testing":
		return NetworkTestnet, true
	case "mainnet", "main", "prod", "production", "live":
		return NetworkMainnet, true
	}
	return "", false
}

// checkEnvironmentsMatch refuses a mixed deployment: minting real units on
// mainnet against sandbox (fake) M-Pesa payments, or charging investors
// real money for testnet units.
func checkEnvironmentsMatch(network, paymentsBaseURL string) error {
	sandbox := strings.Contains(strings.ToLower(paymentsBaseURL), "sandbox")
	switch {
	case network == NetworkMainnet && sandbox:
		return fmt.Errorf("COMET_ENGINE_NETWORK=mainnet but PAYMENTS_BASE_URL is the sandbox (%s): set the live payments URL, or use testnet", paymentsBaseURL)
	case network == NetworkTestnet && !sandbox:
		return fmt.Errorf("COMET_ENGINE_NETWORK=testnet but PAYMENTS_BASE_URL is not the sandbox (%s): investors would pay real money for testnet units", paymentsBaseURL)
	}
	return nil
}
