package storage

import (
	"fmt"

	"gorm.io/gorm"

	migrationfiles "gpt-load/internal/storage/migrations"
)

// Keep fork extensions in a separate ledger so future upstream migration IDs
// cannot collide with the account limits and outbound header snapshots.
type forkSchemaMigration struct {
	ID string `gorm:"column:id;type:varchar(255);primaryKey;not null"`
}

func (forkSchemaMigration) TableName() string { return "fork_schema_migrations" }

var forkMigrations = []migration{
	{ID: migrationfiles.IDForkAccountConcurrency, Up: migrationfiles.UpForkAccountConcurrency, Validate: migrationfiles.ValidateForkAccountConcurrency},
	{ID: migrationfiles.IDForkOutboundHeaders, Up: migrationfiles.UpForkOutboundHeaders, Validate: migrationfiles.ValidateForkOutboundHeaders},
}

func applicationMigrationRegistry(entries []migration) bool {
	if len(entries) != len(migrations) {
		return false
	}
	for i := range entries {
		if entries[i].ID != migrations[i].ID {
			return false
		}
	}
	return true
}

// The deployed fork used upstream slots 0009 and 0010 before upstream added its
// own migrations. Accept only that exact historical prefix, validate its data,
// and let the idempotent extension migrations register in their own ledger.
// This runs inside the same migration lock/SQLite transaction as the main chain.
func reconcileLegacyForkMigrations(db *gorm.DB) error {
	legacy := []string{"0009_add_account_concurrency_limit", "0010_request_log_attempt_outbound_headers"}
	var ids []string
	if err := db.Table(migrationLedgerTable).Order("id ASC").Pluck("id", &ids).Error; err != nil {
		return err
	}
	hasLegacy := false
	for _, id := range ids {
		if id == legacy[0] || id == legacy[1] {
			hasLegacy = true
		}
	}
	if !hasLegacy {
		return nil
	}
	if len(ids) < 9 || len(ids) > 10 {
		return fmt.Errorf("legacy fork migration ledger has an unexpected length")
	}
	for i, id := range ids {
		expected := ""
		if i < 8 {
			expected = migrations[i].ID
		} else {
			expected = legacy[i-8]
		}
		if id != expected {
			return fmt.Errorf("legacy fork migration ledger has unexpected entry %q", id)
		}
	}
	for i := 8; i < len(ids); i++ {
		if err := forkMigrations[i-8].Validate(db); err != nil {
			return fmt.Errorf("validate legacy fork migration %s: %w", ids[i], err)
		}
	}
	return db.Where("id IN ?", legacy).Delete(&schemaMigration{}).Error
}

func applyForkMigrations(db *gorm.DB) error {
	if err := db.AutoMigrate(&forkSchemaMigration{}); err != nil {
		return fmt.Errorf("create fork migration ledger: %w", err)
	}
	for _, entry := range forkMigrations {
		var count int64
		if err := db.Model(&forkSchemaMigration{}).Where("id = ?", entry.ID).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			if err := entry.Up(db); err != nil {
				return fmt.Errorf("apply fork migration %s: %w", entry.ID, err)
			}
		}
		if err := entry.Validate(db); err != nil {
			return fmt.Errorf("validate fork migration %s: %w", entry.ID, err)
		}
		if count == 0 {
			if err := db.Create(&forkSchemaMigration{ID: entry.ID}).Error; err != nil {
				return err
			}
		}
	}
	return nil
}
