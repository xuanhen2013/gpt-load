package migrations

import (
	"fmt"

	"gorm.io/gorm"
)

const (
	// ID0010 adds the bounded outbound desktop identity header snapshot that
	// crossed the wire for one Codex request. Only the whitelisted identity
	// header names are stored; Authorization and proxy credentials never
	// enter this column.
	ID0010 = "0010_request_log_attempt_outbound_headers"

	requestLogAttemptOutboundHeadersColumn0010 = "outbound_identity_headers"
)

type requestLogAttemptOutboundHeaders0010 struct {
	OutboundIdentityHeaders []byte `gorm:"column:outbound_identity_headers;type:json"`
}

func (requestLogAttemptOutboundHeaders0010) TableName() string { return "request_log_attempts" }

// Up0010 adds the nullable outbound identity header snapshot without
// changing existing rows.
func Up0010(db *gorm.DB) error {
	if !db.Migrator().HasTable(&requestLogAttemptOutboundHeaders0010{}) {
		return fmt.Errorf(
			"add request log attempt outbound headers: table %q is missing",
			requestLogAttemptOutboundHeaders0010{}.TableName(),
		)
	}
	if db.Migrator().HasColumn(
		&requestLogAttemptOutboundHeaders0010{},
		requestLogAttemptOutboundHeadersColumn0010,
	) {
		return nil
	}
	if err := db.Migrator().AddColumn(
		&requestLogAttemptOutboundHeaders0010{},
		"OutboundIdentityHeaders",
	); err != nil {
		return fmt.Errorf("add request log attempt outbound headers: %w", err)
	}
	return nil
}

// ValidateRecoverable0010 accepts either side of this idempotent column
// creation; the request_log_attempts table must already exist.
func ValidateRecoverable0010(db *gorm.DB) error {
	if !db.Migrator().HasTable(&requestLogAttemptOutboundHeaders0010{}) {
		return fmt.Errorf(
			"validate recoverable request log attempt outbound headers: table %q is missing",
			requestLogAttemptOutboundHeaders0010{}.TableName(),
		)
	}
	return nil
}

// ValidateCurrent0010 verifies the outbound identity header column is present.
func ValidateCurrent0010(db *gorm.DB) error {
	if !db.Migrator().HasColumn(
		&requestLogAttemptOutboundHeaders0010{},
		requestLogAttemptOutboundHeadersColumn0010,
	) {
		return fmt.Errorf(
			"validate request log attempt outbound headers: column %q is missing",
			requestLogAttemptOutboundHeadersColumn0010,
		)
	}
	return nil
}

// Validate0010 verifies the applied outbound identity header column.
func Validate0010(db *gorm.DB) error { return ValidateCurrent0010(db) }
