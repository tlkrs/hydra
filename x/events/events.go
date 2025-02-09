// Copyright © 2023 Ory Corp
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"context"

	otelattr "go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/ory/fosite"
	"github.com/ory/x/otelx/semconv"
)

const (
	// LoginAccepted will be emitted when the login UI accepts a login request.
	LoginAccepted semconv.Event = "OAuth2LoginAccepted"

	// LoginRejected will be emitted when the login UI rejects a login request.
	LoginRejected semconv.Event = "OAuth2LoginRejected"

	// ConsentAccepted will be emitted when the consent UI accepts a consent request.
	ConsentAccepted semconv.Event = "OAuth2ConsentAccepted"

	// ConsentRejected will be emitted when the consent UI rejects a consent request.
	ConsentRejected semconv.Event = "OAuth2ConsentRejected"

	// ConsentRevoked will be emitted when the user revokes a consent request.
	ConsentRevoked semconv.Event = "OAuth2ConsentRevoked"

	// AccessTokenIssued will be emitted by requests to POST /oauth2/token in case the request was successful.
	AccessTokenIssued semconv.Event = "OAuth2AccessTokenIssued" //nolint:gosec

	// TokenExchangeError will be emitted by requests to POST /oauth2/token in case the request was unsuccessful.
	TokenExchangeError semconv.Event = "OAuth2TokenExchangeError" //nolint:gosec

	// AccessTokenInspected will be emitted by requests to POST /admin/oauth2/introspect.
	AccessTokenInspected semconv.Event = "OAuth2AccessTokenInspected" //nolint:gosec

	// AccessTokenRevoked will be emitted by requests to POST /oauth2/revoke.
	AccessTokenRevoked semconv.Event = "OAuth2AccessTokenRevoked" //nolint:gosec

	// RefreshTokenIssued will be emitted when a refresh token is issued.
	RefreshTokenIssued semconv.Event = "OAuth2RefreshTokenIssued" //nolint:gosec

	// IdentityTokenIssued will be emitted when a refresh token is issued.
	IdentityTokenIssued semconv.Event = "OIDCIdentityTokenIssued" //nolint:gosec
)

const (
	attributeKeyOAuth2ClientID = "OAuth2ClientID"
	attributeKeyOAuth2Subject  = "OAuth2Subject"
)

// WithClientID emits the client ID as part of the event.
func WithClientID(clientID string) trace.EventOption {
	return trace.WithAttributes(otelattr.String(attributeKeyOAuth2ClientID, clientID))
}

// WithSubject emits the subject as part of the event.
func WithSubject(subject string) trace.EventOption {
	return trace.WithAttributes(otelattr.String(attributeKeyOAuth2Subject, subject))
}

// WithRequest emits the subject and client ID from the fosite request as part of the event.
func WithRequest(request fosite.Requester) trace.EventOption {
	var attributes []otelattr.KeyValue
	if client := request.GetClient(); client != nil {
		attributes = append(attributes, otelattr.String(attributeKeyOAuth2ClientID, client.GetID()))
	}
	if session := request.GetSession(); session != nil {
		attributes = append(attributes, otelattr.String(attributeKeyOAuth2Subject, session.GetSubject()))
	}

	return trace.WithAttributes(attributes...)
}

// Trace emits an event with the given attributes.
func Trace(ctx context.Context, event semconv.Event, opts ...trace.EventOption) {
	allOpts := append([]trace.EventOption{trace.WithAttributes(semconv.AttributesFromContext(ctx)...)}, opts...)
	trace.SpanFromContext(ctx).AddEvent(
		string(event),
		allOpts...,
	)
}
