package offerings

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"yeshara.network/marketplace/internal/cometengine"
	"yeshara.network/marketplace/internal/config"
	"yeshara.network/marketplace/internal/database"
)

// Needs a real MongoDB; skipped unless TEST_MONGO_URI is set. Uses its own
// yeshara_offerings_test database, dropped at the start of each test.
func requireMongo(t *testing.T) {
	t.Helper()
	uri := os.Getenv("TEST_MONGO_URI")
	if uri == "" {
		t.Skip("set TEST_MONGO_URI to run the Mongo-backed offering tests")
	}
	if err := database.Init(&config.Config{MongoURI: uri, MongoDB: "yeshara_offerings_test"}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := database.GetDB().Drop(context.Background()); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if err := EnsureIndexes(context.Background()); err != nil {
		t.Fatalf("indexes: %v", err)
	}
}

// fakeEngine stands in for comet-engine: one receipt, per-holder balances,
// and a record of every call so tests can assert what Yeshara sent.
type fakeEngine struct {
	mu        sync.Mutex
	quantity  float64
	redeemed  float64
	balances  map[string]float64
	standards map[string]bool
	calls     []string
	networks  []string
	bodies    map[string]map[string]any

	receiptStatus string
	mintTx        string
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{quantity: 1000, balances: map[string]float64{}, standards: map[string]bool{cometengine.PrimaryStandard: true}, bodies: map[string]map[string]any{}}
}

func (f *fakeEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	key := r.Method + " " + r.URL.Path
	f.calls = append(f.calls, key)
	f.networks = append(f.networks, r.Header.Get("X-Network"))
	f.bodies[key] = body
	reply := func(code int, v any) { w.WriteHeader(code); _ = json.NewEncoder(w).Encode(v) }

	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/tokenize/receipt/standards/"):
		code := strings.TrimPrefix(r.URL.Path, "/api/v1/tokenize/receipt/standards/")
		if !f.standards[code] {
			reply(404, map[string]string{"error": "standard not found"})
			return
		}
		reply(200, map[string]any{"code": code, "symbolPrefix": "YSH", "quantityUnit": "units", "decimals": 2, "chain": "stellar",
			"fields": []map[string]any{{"name": "offeringId", "type": "string", "required": true, "role": "identifier"}}})
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tokenize/receipt/standards":
		f.standards[body["code"].(string)] = true
		reply(201, body)
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tokenize/receipt/deposit":
		reply(201, map[string]any{"id": "rcpt-1", "symbol": "YSHTEST", "status": "pending_verification"})
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tokenize/receipt/rcpt-1/verify":
		// The engine's maker-checker holds the mint; the test approves it
		// by setting receiptStatus = "active".
		reply(202, map[string]any{"status": "pending_approval", "approvalId": "appr-1", "actionScope": "tokenize.receipt.mint", "threshold": 2})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tokenize/receipt/rcpt-1":
		resp := map[string]any{"id": "rcpt-1", "symbol": "YSHTEST", "chain": "stellar", "quantity": f.quantity, "redeemedQuantity": f.redeemed,
			"status": f.receiptStatus, "mintTxHash": f.mintTx}
		if dep := f.bodies["POST /api/v1/tokenize/receipt/deposit"]; dep != nil && dep["shariah"] != nil {
			resp["shariah"] = dep["shariah"]
		}
		reply(200, resp)
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tokenize/receipt/rcpt-1/shariah/certification":
		reply(200, map[string]any{"id": "rcpt-1", "shariah": map[string]any{"structure": "ijarah", "sector": "real_estate", "certification": body}})
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/balance"):
		reply(200, map[string]any{"humanAmount": f.balances[r.URL.Query().Get("externalUserId")]})
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tokenize/receipt/rcpt-1/redeem":
		user, amount := body["externalUserId"].(string), body["amount"].(float64)
		f.balances[user] -= amount
		f.redeemed += amount
		reply(200, map[string]any{"txHash": "burn-" + user})
	default:
		reply(404, map[string]string{"error": "unexpected call " + key})
	}
}

func (f *fakeEngine) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == key {
			n++
		}
	}
	return n
}

func setup(t *testing.T) (*fakeEngine, *cometengine.Client) {
	t.Helper()
	requireMongo(t)
	f := newFakeEngine()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, cometengine.New(srv.URL, "k", "s", config.NetworkTestnet)
}

func createOffering(t *testing.T, ce *cometengine.Client, shariah *cometengine.ShariahProfile) *Offering {
	t.Helper()
	o, err := Create(context.Background(), ce, CreateInput{
		OfferingID: "kilimani-flats", Title: "Kilimani Flats", AssetType: "real_estate", TotalUnits: 1000,
		PricePerUnit: 10, Currency: "USD", Chain: "stellar", IssuerUserID: "yeshara-treasury", Shariah: shariah,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return o
}

// goLive puts the offering in the state TrusteeBTokenize leaves it in.
func goLive(t *testing.T, id primitive.ObjectID) {
	t.Helper()
	if _, err := collection().UpdateOne(context.Background(), map[string]any{"_id": id},
		map[string]any{"$set": map[string]any{"status": StatusLive, "owner_user_id": "owner-1", "reserved_units": 200}}); err != nil {
		t.Fatal(err)
	}
}

func TestRemove(t *testing.T) {
	ctx := context.Background()
	f, ce := setup(t)
	o := createOffering(t, ce, nil)

	if _, err := Remove(ctx, ce, o.ID, "yeshara-treasury", "admin", "test", 0); !errors.Is(err, ErrNotRemovable) {
		t.Fatalf("remove pending offering = %v, want ErrNotRemovable", err)
	}
	goLive(t, o.ID)

	// 1000 minted: 200 owner reserve, 650 unsold, 150 with investors.
	f.balances = map[string]float64{"owner-1": 200, "yeshara-treasury": 650}
	var hold *InvestorsHoldError
	if _, err := Remove(ctx, ce, o.ID, "yeshara-treasury", "admin", "test", 0); !errors.As(err, &hold) || hold.Units != 150 {
		t.Fatalf("remove with investors = %v, want InvestorsHoldError{150}", err)
	}
	if _, err := Remove(ctx, ce, o.ID, "yeshara-treasury", "admin", "test", 2); !errors.Is(err, ErrPendingPayments) {
		t.Fatalf("remove with pending payments = %v", err)
	}
	if f.count("POST /api/v1/tokenize/receipt/rcpt-1/redeem") != 0 {
		t.Fatal("nothing may be burned while investors hold units")
	}

	// Investors redeemed their 150 units back.
	f.redeemed = 150
	removed, err := Remove(ctx, ce, o.ID, "yeshara-treasury", "admin", "sold the building", 0)
	if err != nil {
		t.Fatalf("remove = %v", err)
	}
	if removed.Status != StatusRemoved || removed.RemovedBy != "admin" || len(removed.RemovalBurnTxs) != 2 {
		t.Fatalf("removed = %+v", removed)
	}
	if f.balances["owner-1"] != 0 || f.balances["yeshara-treasury"] != 0 || f.redeemed != 1000 {
		t.Fatalf("after removal balances = %v redeemed = %v", f.balances, f.redeemed)
	}
	if _, err := Remove(ctx, ce, o.ID, "yeshara-treasury", "admin", "again", 0); !errors.Is(err, ErrNotRemovable) {
		t.Fatalf("second remove = %v", err)
	}
	for _, n := range f.networks {
		if n != config.NetworkTestnet {
			t.Fatalf("engine call sent X-Network %q, want testnet", n)
		}
	}
}

func TestShariahOffering(t *testing.T) {
	ctx := context.Background()
	f, ce := setup(t)
	profile := &cometengine.ShariahProfile{Structure: "ijarah", Sector: "real_estate", PurificationRatio: 0.02}

	o := createOffering(t, ce, profile)
	if f.count("POST /api/v1/tokenize/receipt/standards") != 1 || !f.standards[cometengine.ShariahStandard] {
		t.Fatal("the Shariah standard should be created on first use")
	}
	dep := f.bodies["POST /api/v1/tokenize/receipt/deposit"]
	if dep["standardCode"] != cometengine.ShariahStandard || dep["shariah"] == nil {
		t.Fatalf("deposit = %v, want the Shariah standard with the profile", dep)
	}
	if o.Shariah == nil || o.Network != config.NetworkTestnet {
		t.Fatalf("offering = %+v", o)
	}
	if err := o.CheckDistributionType("interest"); err == nil {
		t.Fatal("interest must be refused on a Shariah offering")
	}
	if err := o.CheckDistributionType("rental"); err != nil {
		t.Fatalf("rental = %v", err)
	}

	now := time.Now()
	cert := cometengine.ShariahCertification{Board: "Premier Bank Shariah Advisory Board", CertificateRef: "PB-SSB-2026-014",
		IssuedAt: now.AddDate(0, -1, 0), ExpiresAt: now.AddDate(1, 0, 0)}
	certified, err := CertifyShariah(ctx, ce, o.ID, cert)
	if err != nil {
		t.Fatalf("certify: %v", err)
	}
	if certified.Shariah.Certification == nil || certified.Shariah.Certification.CertificateRef != "PB-SSB-2026-014" {
		t.Fatalf("certified = %+v", certified.Shariah)
	}

	// A second Shariah offering reuses the standard.
	if _, err := collection().DeleteMany(ctx, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	createOffering(t, ce, profile)
	if f.count("POST /api/v1/tokenize/receipt/standards") != 1 {
		t.Fatal("the Shariah standard must not be created twice")
	}

	// Conventional offerings stay in the primary pool and take any label.
	if _, err := collection().DeleteMany(ctx, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	conv := createOffering(t, ce, nil)
	if f.bodies["POST /api/v1/tokenize/receipt/deposit"]["standardCode"] != cometengine.PrimaryStandard {
		t.Fatal("conventional offering must use the primary standard")
	}
	if _, err := CertifyShariah(ctx, ce, conv.ID, cert); !errors.Is(err, ErrNotShariah) {
		t.Fatalf("certify conventional = %v, want ErrNotShariah", err)
	}
	if err := conv.CheckDistributionType("interest"); err != nil {
		t.Fatalf("conventional distribution = %v", err)
	}
}

// With an engine approval policy on receipt minting, Trustee B's first
// tokenize is held (offering stays pending), and the second, after the
// signers approve, records the mint without verifying again.
func TestTrusteeBTokenizeWaitsForEngineApproval(t *testing.T) {
	ctx := context.Background()
	f, ce := setup(t)
	o := createOffering(t, ce, nil)
	if _, err := collection().UpdateOne(ctx, map[string]any{"_id": o.ID}, map[string]any{"$set": map[string]any{"status": StatusPendingTrusteeB}}); err != nil {
		t.Fatal(err)
	}
	f.receiptStatus = "pending_verification"

	_, _, err := TrusteeBTokenize(ctx, ce, o.ID, "trustee-b")
	var pending *cometengine.PendingApprovalError
	if !errors.As(err, &pending) || pending.ApprovalID != "appr-1" {
		t.Fatalf("first tokenize = %v, want PendingApprovalError", err)
	}
	if got, _ := Get(ctx, o.ID); got.Status != StatusPendingTrusteeB {
		t.Fatalf("status after held tokenize = %q, must stay pending_trustee_b", got.Status)
	}

	f.receiptStatus, f.mintTx = "active", "mint-tx-1"
	verifies := f.count("POST /api/v1/tokenize/receipt/rcpt-1/verify")
	live, txHash, err := TrusteeBTokenize(ctx, ce, o.ID, "trustee-b")
	if err != nil || live.Status != StatusLive || txHash != "mint-tx-1" {
		t.Fatalf("second tokenize = %+v, %q, %v", live, txHash, err)
	}
	if f.count("POST /api/v1/tokenize/receipt/rcpt-1/verify") != verifies {
		t.Fatal("an already-minted receipt must not be verified again")
	}
}
