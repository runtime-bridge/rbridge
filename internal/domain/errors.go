// Package domain holds the types and contracts shared by every part of rbridge.
// It must not import any other rbridge package.
package domain

import (
	"errors"
	"fmt"
)

// ErrorCode is a stable, machine-readable error identifier.
type ErrorCode string

const (
	ErrProviderNotFound       ErrorCode = "provider_not_found"
	ErrProviderUnavailable    ErrorCode = "provider_unavailable"
	ErrAuthenticationRequired ErrorCode = "authentication_required"
	ErrCapabilityUnavailable  ErrorCode = "capability_unavailable"
	ErrSkillNotFound          ErrorCode = "skill_not_found"
	ErrSkillInvalid           ErrorCode = "skill_invalid"
	ErrSkillBlocked           ErrorCode = "skill_blocked"
	ErrSkillUnavailable       ErrorCode = "skill_unavailable"
	ErrSkillDigestMismatch    ErrorCode = "skill_digest_mismatch"
	ErrPermissionDenied       ErrorCode = "permission_denied"
	ErrPolicyUnenforceable    ErrorCode = "policy_unenforceable"
	ErrApprovalTimeout        ErrorCode = "approval_timeout"
	ErrExecutionTimeout       ErrorCode = "execution_timeout"
	ErrExecutionFailed        ErrorCode = "execution_failed"
	ErrCancelled              ErrorCode = "cancelled"
	ErrSessionNotFound        ErrorCode = "session_not_found"
	ErrProviderProtocol       ErrorCode = "provider_protocol_error"
	ErrInvalidInput           ErrorCode = "invalid_input"
	ErrInternal               ErrorCode = "internal_error"
)

// ExitCode maps an error code to the process exit code used by the CLI.
func (c ErrorCode) ExitCode() int {
	switch c {
	case ErrProviderNotFound:
		return 10
	case ErrProviderUnavailable:
		return 11
	case ErrAuthenticationRequired:
		return 12
	case ErrCapabilityUnavailable:
		return 13
	case ErrSkillNotFound:
		return 20
	case ErrSkillInvalid:
		return 21
	case ErrSkillBlocked:
		return 22
	case ErrSkillUnavailable:
		return 23
	case ErrSkillDigestMismatch:
		return 24
	case ErrPermissionDenied:
		return 30
	case ErrPolicyUnenforceable:
		return 31
	case ErrApprovalTimeout:
		return 32
	case ErrExecutionTimeout:
		return 40
	case ErrExecutionFailed:
		return 41
	case ErrCancelled:
		return 42
	case ErrSessionNotFound:
		return 50
	case ErrProviderProtocol:
		return 60
	case ErrInvalidInput:
		return 2
	default:
		return 70
	}
}

// Error is the error type returned at service boundaries.
// Message and Details must be safe to show to a user and to log.
type Error struct {
	Code      ErrorCode         `json:"code"`
	Message   string            `json:"message"`
	Retryable bool              `json:"retryable"`
	Provider  string            `json:"provider,omitempty"`
	SessionID string            `json:"session_id,omitempty"`
	Details   map[string]string `json:"details,omitempty"`
	Cause     error             `json:"-"`
}

func (e *Error) Error() string {
	if e.Provider != "" {
		return fmt.Sprintf("%s: %s (provider %s)", e.Code, e.Message, e.Provider)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }

// Errorf builds an Error with a formatted message.
func Errorf(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// CodeOf returns the code of the first Error in err's chain, or ErrInternal.
func CodeOf(err error) ErrorCode {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ErrInternal
}
