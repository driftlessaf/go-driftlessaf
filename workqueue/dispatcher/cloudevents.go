/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package dispatcher

import (
	"context"
	"net/http"
	"time"

	"github.com/chainguard-dev/clog"
	"github.com/chainguard-dev/terraform-infra-common/pkg/httpmetrics"
	cloudevents "github.com/cloudevents/sdk-go/v2"
	cehttp "github.com/cloudevents/sdk-go/v2/protocol/http"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"golang.org/x/sync/errgroup"
	"google.golang.org/api/idtoken"
)

const (
	ceRetryDelay  = 100 * time.Millisecond
	ceMaxRetry    = 3
	ceMaxInflight = 100
	ceSendTimeout = 30 * time.Second

	// ErrorEventType is the CloudEvent type for dispatcher error events.
	ErrorEventType = "dev.chainguard.workqueue.error.v1"
)

// DispatcherErrorEvent is the CloudEvent payload for dispatcher errors.
type DispatcherErrorEvent struct {
	// Key is the workqueue key that failed.
	Key string `json:"key"`

	// Error is the error message.
	Error string `json:"error"`

	// Attempts is the number of times the key has been attempted.
	Attempts int `json:"attempts"`

	// Action describes the disposition (requeued, dead-lettered, dropped).
	Action string `json:"action"`

	// NonRetriableReason is set when Action is "dropped", providing the
	// reason the error was marked non-retriable.
	NonRetriableReason string `json:"nonRetriableReason,omitempty"`
}

// cloudEventErrorEmitter publishes dispatch errors as CloudEvents.
type cloudEventErrorEmitter struct {
	client        cloudevents.Client
	workqueueName string
	eg            errgroup.Group
}

var _ errorEmitter = (*cloudEventErrorEmitter)(nil)

func (e *cloudEventErrorEmitter) emit(ctx context.Context, ec ErrorContext) {
	ce := cloudevents.NewEvent()
	ce.SetID(uuid.New().String())
	ce.SetType(ErrorEventType)
	ce.SetSource(e.workqueueName)
	ce.SetSubject(ec.Key)
	ce.SetTime(time.Now())
	ce.SetExtension("action", ec.Action.String())

	if err := ce.SetData(cloudevents.ApplicationJSON, &DispatcherErrorEvent{
		Key:                ec.Key,
		Error:              ec.Err.Error(),
		Attempts:           ec.Attempts,
		Action:             ec.Action.String(),
		NonRetriableReason: ec.NonRetriableReason,
	}); err != nil {
		clog.ErrorContext(ctx, "failed to set dispatcher error event data",
			"key", ec.Key, "error", err)
		return
	}

	e.eg.Go(func() error {
		sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ceSendTimeout)
		defer cancel()

		rctx := cloudevents.ContextWithRetriesExponentialBackoff(sendCtx, ceRetryDelay, ceMaxRetry)
		if result := e.client.Send(rctx, ce); cloudevents.IsUndelivered(result) || cloudevents.IsNACK(result) {
			clog.ErrorContext(ctx, "failed to deliver dispatcher error event",
				"key", ec.Key,
				"action", ec.Action.String(),
				"error", result)
		}
		return nil
	})
}

func (e *cloudEventErrorEmitter) drain() {
	// eg.Go callbacks always return nil; delivery errors are logged in-band.
	_ = e.eg.Wait()
}

// WithErrorBrokerURL configures the dispatcher to emit errors as CloudEvents
// to the given broker URL. workqueueName is used as the CloudEvent source.
// When brokerURL is empty, error events are disabled (no-op).
//
// The CloudEvents client is constructed eagerly so it is shared across all
// dispatch rounds. If construction fails the option degrades gracefully
// to a no-op with a warning log.
func WithErrorBrokerURL(ctx context.Context, brokerURL, workqueueName string) Option {
	if brokerURL == "" {
		return func(*config) {}
	}

	innerTransport := httpmetrics.ExtractInnerTransport(http.DefaultTransport)
	var baseTransport *http.Transport
	if t, ok := innerTransport.(*http.Transport); ok {
		baseTransport = t.Clone()
	} else {
		baseTransport = &http.Transport{}
	}

	tokenSource, err := idtoken.NewTokenSource(ctx, brokerURL)
	if err != nil {
		clog.WarnContextf(ctx, "Failed to create id token source for error events, disabling: %v", err)
		return func(*config) {}
	}

	ceClient, err := cloudevents.NewClientHTTP(
		cloudevents.WithTarget(brokerURL),
		cehttp.WithClient(http.Client{Transport: httpmetrics.WrapTransport(&oauth2.Transport{
			Source: tokenSource,
			Base:   baseTransport,
		})}),
	)
	if err != nil {
		clog.WarnContextf(ctx, "Failed to create cloudevents client for error events, disabling: %v", err)
		return func(*config) {}
	}

	e := &cloudEventErrorEmitter{
		client:        ceClient,
		workqueueName: workqueueName,
	}
	e.eg.SetLimit(ceMaxInflight)

	clog.InfoContextf(ctx, "Error events enabled: broker=%s workqueue=%s", brokerURL, workqueueName)

	return func(c *config) { c.errors = e }
}
