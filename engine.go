// Package payout provides an idempotent batch settlement and disbursement engine
// with two-phase commit (2PC) state locking and exponential backoff retry queues.
package payout

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sync"
	"time"
)

var (
	// ErrInvalidIdentifier is returned when batchId or idempotencyKey is missing.
	ErrInvalidIdentifier = errors.New("payout-engine: batchId and idempotencyKey are required")

	// ErrInvalidMerchant is returned when recipient.merchantId is empty.
	ErrInvalidMerchant = errors.New("payout-engine: recipient.merchantId is required")

	// ErrInvalidRoutingNumber is returned when the ABA routing number is not 9 digits.
	ErrInvalidRoutingNumber = errors.New("payout-engine: routingNumber must be a valid 9-digit US ABA routing number")

	// ErrInvalidSettlementAmount is returned when minor-unit amounts are non-positive.
	ErrInvalidSettlementAmount = errors.New("payout-engine: settlement amounts must be valid integer minor units")

	// ErrReconciliationMismatch is returned when grossAmount - platformFee != netDisbursement.
	ErrReconciliationMismatch = errors.New("payout-engine: gross amount minus platform fee does not equal net disbursement")
)

var abaRoutingRegex = regexp.MustCompile(`^\d{9}$`)

// CommitPhase represents the Two-Phase Commit (2PC) lifecycle state of a payout instruction.
type CommitPhase string

const (
	PhasePrepared   CommitPhase = "PREPARED_LOCKED"
	PhaseCommitted  CommitPhase = "COMMITTED_SETTLED"
	PhaseRetrying   CommitPhase = "QUEUED_FOR_RETRY"
	PhaseRolledBack CommitPhase = "ROLLED_BACK_FAILED"
)

// Recipient maps to the `recipient` object in the payout-engine payload schema.
type Recipient struct {
	MerchantID      string `json:"merchantId"`
	DestinationType string `json:"destinationType"`
	RoutingNumber   string `json:"routingNumber"`
}

// SettlementBreakdown maps to the integer minor-unit `settlement` object in the payload schema.
type SettlementBreakdown struct {
	GrossAmount     int64 `json:"grossAmount"`
	PlatformFee     int64 `json:"platformFee"`
	NetDisbursement int64 `json:"netDisbursement"`
}

// DisbursementPayload represents the canonical request payload for payout-engine.
type DisbursementPayload struct {
	BatchID        string              `json:"batchId"`
	Currency       string              `json:"currency"`
	Recipient      Recipient           `json:"recipient"`
	Settlement     SettlementBreakdown `json:"settlement"`
	IdempotencyKey string              `json:"idempotencyKey"`
}

// LedgerEntry stores the immutable settlement record and idempotency state lock.
type LedgerEntry struct {
	BatchID         string              `json:"batchId"`
	MerchantID      string              `json:"merchantId"`
	Currency        string              `json:"currency"`
	Settlement      SettlementBreakdown `json:"settlement"`
	IdempotencyKey  string              `json:"idempotencyKey"`
	IdempotencyHash string              `json:"idempotencyHash"`
	Phase           CommitPhase         `json:"phase"`
	Attempts        int                 `json:"attempts"`
	NextRetryAt     *time.Time          `json:"nextRetryAt,omitempty"`
	UpdatedAt       time.Time           `json:"updatedAt"`
}

// RetryTask represents a failed disbursement scheduled in the exponential backoff queue.
type RetryTask struct {
	Payload     DisbursementPayload
	Attempt     int
	ScheduledAt time.Time
	LastError   string
}

// Engine coordinates automated batch disbursements, 2PC state locking, and retry queues.
type Engine struct {
	mu          sync.RWMutex
	ledger      map[string]*LedgerEntry
	retryQueue  []RetryTask
	maxRetries  int
	baseBackoff time.Duration
}

// NewEngine initializes a thread-safe payout-engine instance with bounded retry policies.
func NewEngine(maxRetries int, baseBackoff time.Duration) *Engine {
	if maxRetries <= 0 {
		maxRetries = 5
	}
	if baseBackoff <= 0 {
		baseBackoff = 2 * time.Second
	}
	return &Engine{
		ledger:      make(map[string]*LedgerEntry),
		retryQueue:  make([]RetryTask, 0),
		maxRetries:  maxRetries,
		baseBackoff: baseBackoff,
	}
}

// ExecuteDisbursement executes a Two-Phase Commit (Prepare -> Commit) workflow for a single payout.
func (e *Engine) ExecuteDisbursement(
	payload DisbursementPayload,
	gatewayDispatch func(DisbursementPayload) error,
) (*LedgerEntry, error) {
	if err := ValidatePayload(payload); err != nil {
		return nil, err
	}

	hash := computeIdempotencyHash(payload)

	// Phase 1: Prepare & acquire idempotent state lock
	e.mu.Lock()
	if existing, exists := e.ledger[hash]; exists {
		e.mu.Unlock()
		return existing, nil
	}

	entry := &LedgerEntry{
		BatchID:         payload.BatchID,
		MerchantID:      payload.Recipient.MerchantID,
		Currency:        payload.Currency,
		Settlement:      payload.Settlement,
		IdempotencyKey:  payload.IdempotencyKey,
		IdempotencyHash: hash,
		Phase:           PhasePrepared,
		Attempts:        1,
		UpdatedAt:       time.Now().UTC(),
	}
	e.ledger[hash] = entry
	e.mu.Unlock()

	// Phase 2: Dispatch to banking rail & commit or enqueue with exponential backoff
	err := gatewayDispatch(payload)

	e.mu.Lock()
	defer e.mu.Unlock()

	entry.UpdatedAt = time.Now().UTC()
	if err == nil {
		entry.Phase = PhaseCommitted
		entry.NextRetryAt = nil
		return entry, nil
	}

	if entry.Attempts < e.maxRetries {
		backoff := e.calculateBackoff(entry.Attempts)
		nextRetry := time.Now().UTC().Add(backoff)
		entry.Phase = PhaseRetrying
		entry.NextRetryAt = &nextRetry

		e.retryQueue = append(e.retryQueue, RetryTask{
			Payload:     payload,
			Attempt:     entry.Attempts + 1,
			ScheduledAt: nextRetry,
			LastError:   err.Error(),
		})
	} else {
		entry.Phase = PhaseRolledBack
	}

  
	return entry, err
}

func (e *Engine) calculateBackoff(attempt int) time.Duration {
	multiplier := math.Pow(2, float64(attempt-1))
	return time.Duration(float64(e.baseBackoff) * multiplier)
}

// ValidatePayload enforces schema integrity, ABA routing format, and double-entry fee math.
func ValidatePayload(p DisbursementPayload) error {
	if p.BatchID == "" || p.IdempotencyKey == "" {
		return ErrInvalidIdentifier
	}
	if p.Recipient.MerchantID == "" {
		return ErrInvalidMerchant
	}
	if p.Recipient.DestinationType != "custom_connected_account" && p.Recipient.DestinationType != "standard_bank_account" {
		return fmt.Errorf("payout-engine: unsupported destinationType %q", p.Recipient.DestinationType)
	}
	if !abaRoutingRegex.MatchString(p.Recipient.RoutingNumber) {
		return ErrInvalidRoutingNumber
	}
	if p.Settlement.GrossAmount <= 0 || p.Settlement.PlatformFee < 0 {
		return ErrInvalidSettlementAmount
	}
	if p.Settlement.GrossAmount-p.Settlement.PlatformFee != p.Settlement.NetDisbursement {
		return fmt.Errorf(
			"%w: gross (%d) - fee (%d) != net (%d)",
			ErrReconciliationMismatch,
			p.Settlement.GrossAmount,
			p.Settlement.PlatformFee,
			p.Settlement.NetDisbursement,
		)
	}
	return nil
}

func computeIdempotencyHash(p DisbursementPayload) string {
	raw := fmt.Sprintf(
		"%s:%s:%s:%d:%s",
		p.BatchID,
		p.Recipient.MerchantID,
		p.IdempotencyKey,
		p.Settlement.NetDisbursement,
		p.Currency,
	)
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
