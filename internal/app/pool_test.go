package app

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestAccountUsageCountPersistsAndResetsDaily(t *testing.T) {
	oldPool := pool
	oldPoolPath := poolPath
	t.Cleanup(func() {
		pool = oldPool
		poolPath = oldPoolPath
	})

	poolPath = t.TempDir() + "/accounts.json"
	today := time.Now().Format("2006-01-02")
	account := &Account{
		AccountID:       "usage-test",
		Email:           "usage@example.com",
		Status:          "active",
		UsageCount:      7,
		UsageCountToday: 3,
		UsageDate:       today,
	}
	pool = &AccountPool{Accounts: []*Account{account}}

	bumpUsage(account)
	if account.UsageCountToday != 4 || account.UsageCount != 8 {
		t.Fatalf("usage count = %d today / %d total, want 4 today / 8 total", account.UsageCountToday, account.UsageCount)
	}

	data, err := os.ReadFile(poolPath)
	if err != nil {
		t.Fatalf("read persisted pool: %v", err)
	}
	var persisted AccountPool
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("decode persisted pool: %v", err)
	}
	if len(persisted.Accounts) != 1 || persisted.Accounts[0].UsageCountToday != 4 || persisted.Accounts[0].UsageCount != 8 {
		t.Fatalf("persisted usage count = %+v, want 4 today / 8 total", persisted.Accounts[0])
	}

	account.UsageDate = "2000-01-01"
	bumpUsage(account)
	if account.UsageCountToday != 1 || account.UsageCount != 9 {
		t.Fatalf("after date rollover usage count = %d today / %d total, want 1 today / 9 total", account.UsageCountToday, account.UsageCount)
	}

	listed := ListAccounts()
	if len(listed) != 1 || listed[0].UsageCountToday != 1 || listed[0].UsageCount != 9 {
		t.Fatalf("listed usage count = %+v, want 1 today / 9 total", listed)
	}
}
