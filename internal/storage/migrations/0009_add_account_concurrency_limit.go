package migrations

import "gorm.io/gorm"

const ID0009 = "0009_add_account_concurrency_limit"

type credentialConcurrencyLimit0009 struct {
	CredentialID uint `gorm:"column:credential_id;primaryKey"`
	Limit        int  `gorm:"column:limit;not null"`
}

func (credentialConcurrencyLimit0009) TableName() string { return "credential_concurrency_limits" }

func Up0009(db *gorm.DB) error       { return db.AutoMigrate(&credentialConcurrencyLimit0009{}) }
func Validate0009(db *gorm.DB) error { return ValidateCurrent0009(db) }
func ValidateCurrent0009(db *gorm.DB) error {
	if !db.Migrator().HasTable(&credentialConcurrencyLimit0009{}) {
		return gorm.ErrInvalidDB
	}
	return nil
}
func ValidateRecoverable0009(db *gorm.DB) error { return ValidateCurrent0009(db) }
