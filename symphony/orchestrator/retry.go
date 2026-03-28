package orchestrator

import (
	"time"

	"github.com/bazelment/yoloswe/symphony/model"
)

// scheduleRetry creates or replaces a retry entry for an issue.
// Cancels any existing timer for the same issue.
// Spec Section 8.4.
func (o *Orchestrator) scheduleRetry(issueID, identifier string, attempt int, errorMsg string, continuation bool) {
	// Cancel existing timer if present.
	if existing, ok := o.retryTimerMap[issueID]; ok {
		existing.Stop()
		delete(o.retryTimerMap, issueID)
	}

	var delayMs int64
	if continuation {
		delayMs = 1000 // Short fixed delay for continuation retries.
	} else {
		cfg := o.cfg()
		delayMs = backoffDelayMs(attempt, cfg.MaxRetryBackoffMs)
	}

	o.nextGen++
	gen := o.nextGen

	entry := &model.RetryEntry{
		IssueID:    issueID,
		Identifier: identifier,
		Attempt:    attempt,
		DueAtMs:    o.clock.Now().Add(time.Duration(delayMs) * time.Millisecond).UnixMilli(),
		Generation: gen,
		Error:      errorMsg,
	}
	o.retryAttempts[issueID] = entry

	timer := o.clock.AfterFunc(time.Duration(delayMs)*time.Millisecond, func() {
		o.retryTimers <- retryFired{IssueID: issueID, Generation: gen}
	})
	o.retryTimerMap[issueID] = timer

	o.logger.Info("retry scheduled",
		"issue_id", issueID,
		"identifier", identifier,
		"attempt", attempt,
		"delay_ms", delayMs,
		"continuation", continuation,
	)
}

// backoffDelayMs calculates exponential backoff delay.
// Formula: min(10000 * 2^(attempt-1), max_retry_backoff_ms).
// Spec Section 8.4.
func backoffDelayMs(attempt int, maxBackoffMs int) int64 {
	if attempt < 1 {
		attempt = 1
	}
	delay := int64(10000)
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay > int64(maxBackoffMs) {
			return int64(maxBackoffMs)
		}
	}
	if delay > int64(maxBackoffMs) {
		return int64(maxBackoffMs)
	}
	return delay
}
