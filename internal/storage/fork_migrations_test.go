package storage

import (
	"testing"

	migrationfiles "gpt-load/internal/storage/migrations"
	"gpt-load/internal/storage/models"
)

func TestLegacyForkUpgradePreservesExtensionsAndLogs(t *testing.T) {
	db := openInternalMigrationTestDatabase(t)
	if err := applyMigrationRegistry(db, migrations[:8]); err != nil {
		t.Fatal(err)
	}
	legacyIDs := []string{"0009_add_account_concurrency_limit", "0010_request_log_attempt_outbound_headers"}
	for i, entry := range forkMigrations {
		if err := entry.Up(db); err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&schemaMigration{ID: legacyIDs[i]}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&models.CredentialConcurrencyLimit{CredentialID: 5, Limit: 4}).Error; err != nil {
		t.Fatal(err)
	}
	request := models.RequestLog{
		ID: "00000000-0000-4000-8000-000000000024", CompletedAtMS: 1000,
		AccessKeyID: 1, Protocol: "openai-responses", ClientModel: "model", UpstreamModel: "model",
		ModelConsistency: "not_applicable", Status: "success", StatusCode: 200, DurationMs: 1,
		UsageState: "not_applicable", CostState: "not_applicable", PricingCompleteness: "not_applicable",
	}
	if err := db.Omit(
		"RequestAudit", "AuditCostNanoUSD", "AuditPricingCompleteness", "AffinityKind", "AutoDecision",
		"DecisionModel", "DecisionCostNanoUSD", "DecisionPricingCompleteness", "DecisionGroupID",
		"DecisionChannelID", "DecisionCredentialID",
	).Create(&request).Error; err != nil {
		t.Fatal(err)
	}
	headers := models.JSON(`{"originator":"fixture-desktop"}`)
	attempt := models.RequestLogAttempt{
		RequestID: request.ID, Sequence: 1, GroupID: 1, CredentialID: 5,
		FailureCategory: "ok", Action: "terminate", OutboundIdentityHeaders: headers,
	}
	if err := db.Omit("CooldownUntilMS").Create(&attempt).Error; err != nil {
		t.Fatal(err)
	}
	for run := 0; run < 2; run++ {
		if err := AutoMigrate(db); err != nil {
			t.Fatalf("upgrade/restart %d: %v", run, err)
		}
	}
	var limit models.CredentialConcurrencyLimit
	if err := db.First(&limit, "credential_id = ?", 5).Error; err != nil || limit.Limit != 4 {
		t.Fatalf("concurrency limit was not preserved: %#v, %v", limit, err)
	}
	var got models.RequestLogAttempt
	if err := db.First(&got, "request_id = ? AND sequence = ?", request.ID, 1).Error; err != nil {
		t.Fatal(err)
	}
	if string(got.OutboundIdentityHeaders) != string(headers) {
		t.Fatalf("outbound snapshot changed: %s", got.OutboundIdentityHeaders)
	}
	var upstreamCount, forkCount int64
	if err := db.Model(&schemaMigration{}).Count(&upstreamCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&forkSchemaMigration{}).Count(&forkCount).Error; err != nil {
		t.Fatal(err)
	}
	if upstreamCount != int64(len(migrations)) || forkCount != int64(len(forkMigrations)) {
		t.Fatalf("ledger counts: upstream=%d, fork=%d", upstreamCount, forkCount)
	}
}

func TestLegacyForkUpgradeRejectsMissingExtensionBeforeChangingLedger(t *testing.T) {
	db := openInternalMigrationTestDatabase(t)
	if err := applyMigrationRegistry(db, migrations[:8]); err != nil {
		t.Fatal(err)
	}
	if err := migrationfiles.UpForkAccountConcurrency(db); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"0009_add_account_concurrency_limit", "0010_request_log_attempt_outbound_headers"} {
		if err := db.Create(&schemaMigration{ID: id}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := AutoMigrate(db); err == nil {
		t.Fatal("accepted legacy ledger with a missing outbound headers column")
	}
	var count int64
	if err := db.Model(&schemaMigration{}).Count(&count).Error; err != nil || count != 10 {
		t.Fatalf("legacy ledger changed after failed validation: %d, %v", count, err)
	}
}
