package config

import "testing"

func TestNormalizeNetwork(t *testing.T) {
	tests := []struct {
		in, want string
		ok       bool
	}{
		{"testnet", NetworkTestnet, true},
		{"Test Net", NetworkTestnet, true},
		{"sandbox", NetworkTestnet, true},
		{"mainnet", NetworkMainnet, true},
		{"Production", NetworkMainnet, true},
		{"live", NetworkMainnet, true},
		{"staging", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		got, ok := NormalizeNetwork(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("NormalizeNetwork(%q) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestCheckEnvironmentsMatch(t *testing.T) {
	const sandbox = "https://sandbox.payments.mamlakapsp.com/api/v1"
	const live = "https://payments.mamlakapsp.com/api/v1"
	tests := []struct {
		network, url string
		wantErr      bool
	}{
		{NetworkTestnet, sandbox, false},
		{NetworkMainnet, live, false},
		{NetworkMainnet, sandbox, true}, // real units, fake payments
		{NetworkTestnet, live, true},    // real money, test units
	}
	for _, tt := range tests {
		if err := checkEnvironmentsMatch(tt.network, tt.url); (err != nil) != tt.wantErr {
			t.Errorf("checkEnvironmentsMatch(%s, %s) = %v, wantErr %v", tt.network, tt.url, err, tt.wantErr)
		}
	}
}
