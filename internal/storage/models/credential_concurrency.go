package models

// CredentialConcurrencyLimit stores the optional account-level override in a
// separate table so old credential rows and rollback migrations remain readable.
type CredentialConcurrencyLimit struct {
	CredentialID uint `gorm:"column:credential_id;primaryKey"`
	Limit        int  `gorm:"column:limit;not null"`
}

func (CredentialConcurrencyLimit) TableName() string { return "credential_concurrency_limits" }
