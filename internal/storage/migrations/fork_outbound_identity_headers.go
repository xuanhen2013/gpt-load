package migrations

import (
	"fmt"

	"gorm.io/gorm"
)

const (
	// IDForkOutboundHeaders adds the bounded outbound desktop identity header snapshot that
	// crossed the wire for one Codex request. Only the whitelisted identity
	// header names are stored; Authorization and proxy credentials never
	// enter this column.
	IDForkOutboundHeaders = "0002_request_log_attempt_outbound_headers"

	requestLogAttemptOutboundHeadersColumnForkOutboundHeaders = "outbound_identity_headers"
)

type requestLogAttemptOutboundHeadersForkOutboundHeaders struct {
	OutboundIdentityHeaders []byte `gorm:"column:outbound_identity_headers;type:json"`
}

func (requestLogAttemptOutboundHeadersForkOutboundHeaders) TableName() string {
	return "request_log_attempts"
}

// UpForkOutboundHeaders adds the nullable outbound identity header snapshot without
// changing existing rows.
func UpForkOutboundHeaders(db *gorm.DB) error {
	if !db.Migrator().HasTable(&requestLogAttemptOutboundHeadersForkOutboundHeaders{}) {
		return fmt.Errorf(
			"add request log attempt outbound headers: table %q is missing",
			requestLogAttemptOutboundHeadersForkOutboundHeaders{}.TableName(),
		)
	}
	if db.Migrator().HasColumn(
		&requestLogAttemptOutboundHeadersForkOutboundHeaders{},
		requestLogAttemptOutboundHeadersColumnForkOutboundHeaders,
	) {
		return nil
	}
	if err := db.Migrator().AddColumn(
		&requestLogAttemptOutboundHeadersForkOutboundHeaders{},
		"OutboundIdentityHeaders",
	); err != nil {
		return fmt.Errorf("add request log attempt outbound headers: %w", err)
	}
	return nil
}

// ValidateRecoverableForkOutboundHeaders accepts either side of this idempotent column
// creation; the request_log_attempts table must already exist.
func ValidateRecoverableForkOutboundHeaders(db *gorm.DB) error {
	if !db.Migrator().HasTable(&requestLogAttemptOutboundHeadersForkOutboundHeaders{}) {
		return fmt.Errorf(
			"validate recoverable request log attempt outbound headers: table %q is missing",
			requestLogAttemptOutboundHeadersForkOutboundHeaders{}.TableName(),
		)
	}
	return nil
}

// ValidateCurrentForkOutboundHeaders verifies the outbound identity header column is present.
func ValidateCurrentForkOutboundHeaders(db *gorm.DB) error {
	if !db.Migrator().HasColumn(
		&requestLogAttemptOutboundHeadersForkOutboundHeaders{},
		requestLogAttemptOutboundHeadersColumnForkOutboundHeaders,
	) {
		return fmt.Errorf(
			"validate request log attempt outbound headers: column %q is missing",
			requestLogAttemptOutboundHeadersColumnForkOutboundHeaders,
		)
	}
	return nil
}

// ValidateForkOutboundHeaders verifies the applied outbound identity header column.
func ValidateForkOutboundHeaders(db *gorm.DB) error { return ValidateCurrentForkOutboundHeaders(db) }
