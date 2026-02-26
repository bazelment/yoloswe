package cursor

import "context"

// Query sends a one-shot prompt and returns the result synchronously.
func Query(ctx context.Context, prompt string, opts ...SessionOption) (*QueryResult, error) {
	session := NewSession(prompt, opts...)
	if err := session.Start(ctx); err != nil {
		return nil, err
	}
	defer session.Stop()

	result := &QueryResult{}
	for event := range session.Events() {
		switch e := event.(type) {
		case ReadyEvent:
			result.SessionID = e.SessionID
		case TurnCompleteEvent:
			result.Success = e.Success
			result.DurationMs = e.DurationMs
			if e.Error != nil {
				return nil, e.Error
			}
			return result, nil
		case TextEvent:
			result.Text = e.FullText
		case ErrorEvent:
			return nil, e.Error
		}
	}

	// Channel closed without a result event
	if result.Text != "" {
		return result, nil
	}
	return nil, ErrSessionClosed
}

// QueryStream sends a one-shot prompt and returns an event channel.
// The caller should range over the channel until it closes.
func QueryStream(ctx context.Context, prompt string, opts ...SessionOption) (<-chan Event, error) {
	session := NewSession(prompt, opts...)
	if err := session.Start(ctx); err != nil {
		return nil, err
	}

	out := make(chan Event, session.config.EventBufferSize)
	go func() {
		defer close(out)
		defer session.Stop()
		for evt := range session.Events() {
			select {
			case out <- evt:
			case <-ctx.Done():
				return
			}
			if _, ok := evt.(TurnCompleteEvent); ok {
				return
			}
		}
	}()

	return out, nil
}
