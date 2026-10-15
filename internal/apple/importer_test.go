package apple

import (
	"testing"
)

func TestParseCredentialsWithCodeURL(t *testing.T) {
	line := "demo_user@example.com----DemoPassword123----https://devices.example.com/api/getcode?id=100001"
	acc, err := ParseCredentials(line)
	if err != nil {
		t.Fatalf("ParseCredentials failed: %v", err)
	}
	if acc.Email != "demo_user@example.com" {
		t.Errorf("expected email demo_user@example.com, got %s", acc.Email)
	}
	if acc.Password != "DemoPassword123" {
		t.Errorf("expected password DemoPassword123, got %s", acc.Password)
	}
	if acc.CodeURL != "https://devices.example.com/api/getcode?id=100001" {
		t.Errorf("expected code_url https://devices.example.com/api/getcode?id=100001, got %s", acc.CodeURL)
	}
}

func TestSmartBatchImportWithCodeURL(t *testing.T) {
	content := `
# test accounts
demo_user@example.com----DemoPassword123----https://devices.example.com/api/getcode?id=100001
user2@icloud.com----Pass222
`
	res := SmartBatchImport(content)
	if res.SuccessCount != 2 {
		t.Fatalf("expected 2 accounts imported, got %d (errors: %v)", res.SuccessCount, res.Errors)
	}
	if res.Accounts[0].CodeURL != "https://devices.example.com/api/getcode?id=100001" {
		t.Errorf("first account code_url mismatch: %s", res.Accounts[0].CodeURL)
	}
	if res.Accounts[1].CodeURL != "" {
		t.Errorf("second account should have no code_url, got %s", res.Accounts[1].CodeURL)
	}
}
