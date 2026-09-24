package migrations

import "gorm.io/gorm"

const IDForkAccountConcurrency = "0001_account_concurrency_limit"

type credentialConcurrencyLimitForkAccountConcurrency struct {
	CredentialID uint `gorm:"column:credential_id;primaryKey"`
	Limit        int  `gorm:"column:limit;not null"`
}

func (credentialConcurrencyLimitForkAccountConcurrency) TableName() string {
	return "credential_concurrency_limits"
}

func UpForkAccountConcurrency(db *gorm.DB) error {
	return db.AutoMigrate(&credentialConcurrencyLimitForkAccountConcurrency{})
}
func ValidateForkAccountConcurrency(db *gorm.DB) error {
	return ValidateCurrentForkAccountConcurrency(db)
}
func ValidateCurrentForkAccountConcurrency(db *gorm.DB) error {
	if !db.Migrator().HasTable(&credentialConcurrencyLimitForkAccountConcurrency{}) {
		return gorm.ErrInvalidDB
	}
	return nil
}
func ValidateRecoverableForkAccountConcurrency(db *gorm.DB) error {
	return ValidateCurrentForkAccountConcurrency(db)
}
