package store

import "testing"

func TestIsSecretPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"InternetGatewayDevice.ManagementServer.Password", true},
		{"InternetGatewayDevice.ManagementServer.ConnectionRequestPassword", true},
		{"InternetGatewayDevice.X.KeyPassphrase", true},
		{"InternetGatewayDevice.X.PreSharedKey", true},
		{"InternetGatewayDevice.X.WEPKey", true},
		{"InternetGatewayDevice.X.ParameterKey", false},
		{"InternetGatewayDevice.ManagementServer.ConnectionRequestURL", false},
		{"InternetGatewayDevice.X.Username", false},
		{"Password", true},
		{"ParameterKey", false},
	}
	for _, tc := range tests {
		if got := IsSecretPath(tc.path); got != tc.want {
			t.Errorf("IsSecretPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}
