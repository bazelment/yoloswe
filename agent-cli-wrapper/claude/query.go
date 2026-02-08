package claude

import "context"

// QueryResult extends TurnResult with session metadata.
type QueryResult struct {
	SessionID string
	TurnResult
}

// Query sends a one-shot prompt and returns the result.
// Defaults to PermissionModeBypass if no permission mode is specified.
func Query(ctx context.Context, prompt string, opts ...SessionOption) (*QueryResult, error) {
	// Apply bypass default if permission mode is not set
	config := defaultConfig()
	for _, opt := range opts {
		opt(&config)
	}
	if config.PermissionMode == PermissionModeDefault {
		opts = append([]SessionOption{WithPermissionMode(PermissionModeBypass)}, opts...)
	}

	session := NewSession(opts...)
	if err := session.Start(ctx); err != nil {
		return nil, err
	}
	defer session.Stop()

	result, err := session.Ask(ctx, prompt)
	if err != nil {
		return nil, err
	}

	info := session.Info()
	sessionID := ""
	if info != nil {
		sessionID = info.SessionID
	}

	return &QueryResult{
		TurnResult: *result,
		SessionID:  sessionID,
	}, nil
}

// QueryStream sends a one-shot prompt and returns an event channel.
// The caller should range over the channel until it closes.
// Defaults to PermissionModeBypass if no permission mode is specified.
func QueryStream(ctx context.Context, prompt string, opts ...SessionOption) (<-chan Event, error) {
	// Apply bypass default if permission mode is not set
	config := defaultConfig()
	for _, opt := range opts {
		opt(&config)
	}
	if config.PermissionMode == PermissionModeDefault {
		opts = append([]SessionOption{WithPermissionMode(PermissionModeBypass)}, opts...)
	}

	session := NewSession(opts...)
	if err := session.Start(ctx); err != nil {
		return nil, err
	}

	// Send the message
	if _, err := session.SendMessage(ctx, prompt); err != nil {
		session.Stop()
		return nil, err
	}

	// Create output channel that proxies events and handles cleanup.
	// The goroutine respects ctx cancellation to avoid blocking forever
	// if the caller stops consuming from out.
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
