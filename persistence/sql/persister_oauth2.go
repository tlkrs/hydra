// Copyright © 2022 Ory Corp
// SPDX-License-Identifier: Apache-2.0

package sql

import (
	"context"
	"crypto/sha512"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/gofrs/uuid"
	"github.com/pkg/errors"
	"github.com/tidwall/gjson"

	"github.com/ory/fosite"
	"github.com/ory/fosite/storage"
	"github.com/ory/hydra/v2/oauth2"
	"github.com/ory/hydra/v2/x/events"
	"github.com/ory/x/errorsx"
	"github.com/ory/x/otelx"
	"github.com/ory/x/sqlcon"
	"github.com/ory/x/stringsx"
)

var _ oauth2.AssertionJWTReader = &Persister{}
var _ storage.Transactional = &Persister{}

type (
	tableName        string
	OAuth2RequestSQL struct {
		ID                string         `db:"signature"`
		NID               uuid.UUID      `db:"nid"`
		Request           string         `db:"request_id"`
		ConsentChallenge  sql.NullString `db:"challenge_id"`
		RequestedAt       time.Time      `db:"requested_at"`
		Client            string         `db:"client_id"`
		Scopes            string         `db:"scope"`
		GrantedScope      string         `db:"granted_scope"`
		RequestedAudience string         `db:"requested_audience"`
		GrantedAudience   string         `db:"granted_audience"`
		Form              string         `db:"form_data"`
		Subject           string         `db:"subject"`
		Active            bool           `db:"active"`
		Session           []byte         `db:"session_data"`
		Table             tableName      `db:"-"`
	}
)

const (
	sqlTableOpenID  tableName = "oidc"
	sqlTableAccess  tableName = "access"
	sqlTableRefresh tableName = "refresh"
	sqlTableCode    tableName = "code"
	sqlTablePKCE    tableName = "pkce"
)

func (r OAuth2RequestSQL) TableName() string {
	return "hydra_oauth2_" + string(r.Table)
}

func (p *Persister) sqlSchemaFromRequest(ctx context.Context, rawSignature string, r fosite.Requester, table tableName) (*OAuth2RequestSQL, error) {
	subject := ""
	if r.GetSession() == nil {
		p.l.Debugf("Got an empty session in sqlSchemaFromRequest")
	} else {
		subject = r.GetSession().GetSubject()
	}

	session, err := json.Marshal(r.GetSession())
	if err != nil {
		return nil, errorsx.WithStack(err)
	}

	if p.config.EncryptSessionData(ctx) {
		ciphertext, err := p.r.KeyCipher().Encrypt(ctx, session, nil)
		if err != nil {
			return nil, errorsx.WithStack(err)
		}
		session = []byte(ciphertext)
	}

	var challenge sql.NullString
	rr, ok := r.GetSession().(*oauth2.Session)
	if !ok && r.GetSession() != nil {
		return nil, errors.Errorf("Expected request to be of type *Session, but got: %T", r.GetSession())
	} else if ok {
		if len(rr.ConsentChallenge) > 0 {
			challenge = sql.NullString{Valid: true, String: rr.ConsentChallenge}
		}
	}

	return &OAuth2RequestSQL{
		Request:           r.GetID(),
		ConsentChallenge:  challenge,
		ID:                p.hashSignature(ctx, rawSignature, table),
		RequestedAt:       r.GetRequestedAt(),
		Client:            r.GetClient().GetID(),
		Scopes:            strings.Join(r.GetRequestedScopes(), "|"),
		GrantedScope:      strings.Join(r.GetGrantedScopes(), "|"),
		GrantedAudience:   strings.Join(r.GetGrantedAudience(), "|"),
		RequestedAudience: strings.Join(r.GetRequestedAudience(), "|"),
		Form:              r.GetRequestForm().Encode(),
		Session:           session,
		Subject:           subject,
		Active:            true,
		Table:             table,
	}, nil
}

func (r *OAuth2RequestSQL) toRequest(ctx context.Context, session fosite.Session, p *Persister) (_ *fosite.Request, err error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.toRequest")
	defer otelx.End(span, &err)

	sess := r.Session
	if !gjson.ValidBytes(sess) {
		var err error
		sess, err = p.r.KeyCipher().Decrypt(ctx, string(sess), nil)
		if err != nil {
			return nil, errorsx.WithStack(err)
		}
	}

	if session != nil {
		if err := json.Unmarshal(sess, session); err != nil {
			return nil, errorsx.WithStack(err)
		}
	} else {
		p.l.Debugf("Got an empty session in toRequest")
	}

	c, err := p.GetClient(ctx, r.Client)
	if err != nil {
		return nil, err
	}

	val, err := url.ParseQuery(r.Form)
	if err != nil {
		return nil, errorsx.WithStack(err)
	}

	return &fosite.Request{
		ID:                r.Request,
		RequestedAt:       r.RequestedAt,
		Client:            c,
		RequestedScope:    stringsx.Splitx(r.Scopes, "|"),
		GrantedScope:      stringsx.Splitx(r.GrantedScope, "|"),
		RequestedAudience: stringsx.Splitx(r.RequestedAudience, "|"),
		GrantedAudience:   stringsx.Splitx(r.GrantedAudience, "|"),
		Form:              val,
		Session:           session,
	}, nil
}

// SignatureHash hashes the signature to prevent errors where the signature is
// longer than 128 characters (and thus doesn't fit into the pk).
func SignatureHash(signature string) string {
	return fmt.Sprintf("%x", sha512.Sum384([]byte(signature)))
}

// hashSignature prevents errors where the signature is longer than 128 characters (and thus doesn't fit into the pk).
func (p *Persister) hashSignature(_ context.Context, signature string, table tableName) string {
	if table == sqlTableAccess {
		return SignatureHash(signature)
	}
	return signature
}

func (p *Persister) ClientAssertionJWTValid(ctx context.Context, jti string) (err error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.ClientAssertionJWTValid")
	defer otelx.End(span, &err)

	j, err := p.GetClientAssertionJWT(ctx, jti)
	if errors.Is(err, sqlcon.ErrNoRows) {
		// the jti is not known => valid
		return nil
	} else if err != nil {
		return err
	}
	if j.Expiry.After(time.Now()) {
		// the jti is not expired yet => invalid
		return errorsx.WithStack(fosite.ErrJTIKnown)
	}
	// the jti is expired => valid
	return nil
}

func (p *Persister) SetClientAssertionJWT(ctx context.Context, jti string, exp time.Time) (err error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.SetClientAssertionJWT")
	defer otelx.End(span, &err)

	// delete expired; this cleanup spares us the need for a background worker
	if err := p.QueryWithNetwork(ctx).Where("expires_at < CURRENT_TIMESTAMP").Delete(&oauth2.BlacklistedJTI{}); err != nil {
		return sqlcon.HandleError(err)
	}

	if err := p.SetClientAssertionJWTRaw(ctx, oauth2.NewBlacklistedJTI(jti, exp)); errors.Is(err, sqlcon.ErrUniqueViolation) {
		// found a jti
		return errorsx.WithStack(fosite.ErrJTIKnown)
	} else if err != nil {
		return err
	}

	// setting worked without a problem
	return nil
}

func (p *Persister) GetClientAssertionJWT(ctx context.Context, j string) (_ *oauth2.BlacklistedJTI, err error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.GetClientAssertionJWT")
	defer otelx.End(span, &err)

	jti := oauth2.NewBlacklistedJTI(j, time.Time{})
	return jti, sqlcon.HandleError(p.QueryWithNetwork(ctx).Find(jti, jti.ID))
}

func (p *Persister) SetClientAssertionJWTRaw(ctx context.Context, jti *oauth2.BlacklistedJTI) (err error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.SetClientAssertionJWTRaw")
	defer otelx.End(span, &err)

	return sqlcon.HandleError(p.CreateWithNetwork(ctx, jti))
}

func (p *Persister) createSession(ctx context.Context, signature string, requester fosite.Requester, table tableName) (err error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.createSession")
	defer otelx.End(span, &err)

	req, err := p.sqlSchemaFromRequest(ctx, signature, requester, table)
	if err != nil {
		return err
	}

	if err = sqlcon.HandleError(p.CreateWithNetwork(ctx, req)); errors.Is(err, sqlcon.ErrConcurrentUpdate) {
		return errors.Wrap(fosite.ErrSerializationFailure, err.Error())
	} else if err != nil {
		return err
	}
	return nil
}

func (p *Persister) findSessionBySignature(ctx context.Context, rawSignature string, session fosite.Session, table tableName) (_ fosite.Requester, err error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.findSessionBySignature")
	defer otelx.End(span, &err)

	r := OAuth2RequestSQL{Table: table}

	// We look for the signature as well as the hash of the signature here.
	// This is because we now always store the hash of the signature in the database,
	// regardless of the type of the signature. In previous versions, we only stored
	// the hash of the signature for JWT tokens.
	//
	// This code will be removed in a future version.
	err = p.QueryWithNetwork(ctx).Where("signature IN (?, ?)", rawSignature, SignatureHash(rawSignature)).First(&r)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errorsx.WithStack(fosite.ErrNotFound)
	} else if err != nil {
		return nil, sqlcon.HandleError(err)
	} else if !r.Active {
		fr, err := r.toRequest(ctx, session, p)
		if err != nil {
			return nil, err
		} else if table == sqlTableCode {
			return fr, errorsx.WithStack(fosite.ErrInvalidatedAuthorizeCode)
		}
		return fr, errorsx.WithStack(fosite.ErrInactiveToken)
	}

	return r.toRequest(ctx, session, p)
}

func (p *Persister) deleteSessionBySignature(ctx context.Context, signature string, table tableName) (err error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.deleteSessionBySignature")
	defer otelx.End(span, &err)

	signature = p.hashSignature(ctx, signature, table)

	// We look for the signature as well as the hash of the signature here.
	// This is because we now always store the hash of the signature in the database,
	// regardless of the type of the signature. In previous versions, we only stored
	// the hash of the signature for JWT tokens.
	//
	// This code will be removed in a future version.
	err = sqlcon.HandleError(
		p.QueryWithNetwork(ctx).
			Where("signature IN (?, ?)", signature, SignatureHash(signature)).
			Delete(&OAuth2RequestSQL{Table: table}))

	if errors.Is(err, sqlcon.ErrNoRows) {
		return errorsx.WithStack(fosite.ErrNotFound)
	} else if errors.Is(err, sqlcon.ErrConcurrentUpdate) {
		return errors.Wrap(fosite.ErrSerializationFailure, err.Error())
	} else if err != nil {
		return err
	}
	return nil
}

func (p *Persister) deleteSessionByRequestID(ctx context.Context, id string, table tableName) (err error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.deleteSessionByRequestID")
	defer otelx.End(span, &err)

	/* #nosec G201 table is static */
	if err := p.QueryWithNetwork(ctx).
		Where("request_id=?", id).
		Delete(&OAuth2RequestSQL{Table: table}); errors.Is(err, sql.ErrNoRows) {
		return errorsx.WithStack(fosite.ErrNotFound)
	} else if err := sqlcon.HandleError(err); err != nil {
		if errors.Is(err, sqlcon.ErrConcurrentUpdate) {
			return errors.Wrap(fosite.ErrSerializationFailure, err.Error())
		} else if strings.Contains(err.Error(), "Error 1213") { // InnoDB Deadlock?
			return errors.Wrap(fosite.ErrSerializationFailure, err.Error())
		}
		return err
	}
	return nil
}

func (p *Persister) deactivateSessionByRequestID(ctx context.Context, id string, table tableName) (err error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.deactivateSessionByRequestID")
	defer otelx.End(span, &err)

	/* #nosec G201 table is static */
	return sqlcon.HandleError(
		p.Connection(ctx).
			RawQuery(
				fmt.Sprintf("UPDATE %s SET active=false WHERE request_id=? AND nid = ? AND active=true", OAuth2RequestSQL{Table: table}.TableName()),
				id,
				p.NetworkID(ctx),
			).
			Exec(),
	)
}

func (p *Persister) CreateAuthorizeCodeSession(ctx context.Context, signature string, requester fosite.Requester) error {
	return otelx.WithSpan(ctx, "persistence.sql.CreateAuthorizeCodeSession", func(ctx context.Context) error {
		return p.createSession(ctx, signature, requester, sqlTableCode)
	})
}

func (p *Persister) GetAuthorizeCodeSession(ctx context.Context, signature string, session fosite.Session) (request fosite.Requester, err error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.GetAuthorizeCodeSession")
	defer otelx.End(span, &err)

	return p.findSessionBySignature(ctx, signature, session, sqlTableCode)
}

func (p *Persister) InvalidateAuthorizeCodeSession(ctx context.Context, signature string) (err error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.InvalidateAuthorizeCodeSession")
	defer otelx.End(span, &err)

	/* #nosec G201 table is static */
	return sqlcon.HandleError(
		p.Connection(ctx).
			RawQuery(
				fmt.Sprintf("UPDATE %s SET active=false WHERE signature=? AND nid = ?", OAuth2RequestSQL{Table: sqlTableCode}.TableName()),
				signature,
				p.NetworkID(ctx),
			).
			Exec(),
	)
}

func (p *Persister) CreateAccessTokenSession(ctx context.Context, signature string, requester fosite.Requester) error {
	events.Trace(ctx, events.AccessTokenIssued, events.WithRequest(requester))
	return otelx.WithSpan(ctx, "persistence.sql.CreateAccessTokenSession", func(ctx context.Context) error {
		return p.createSession(ctx, signature, requester, sqlTableAccess)
	})
}

func (p *Persister) GetAccessTokenSession(ctx context.Context, signature string, session fosite.Session) (request fosite.Requester, err error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.GetAccessTokenSession")
	defer otelx.End(span, &err)
	return p.findSessionBySignature(ctx, signature, session, sqlTableAccess)
}

func (p *Persister) DeleteAccessTokenSession(ctx context.Context, signature string) error {
	return otelx.WithSpan(ctx, "persistence.sql.DeleteAccessTokenSession", func(ctx context.Context) error {
		return p.deleteSessionBySignature(ctx, signature, sqlTableAccess)
	})
}

func (p *Persister) CreateRefreshTokenSession(ctx context.Context, signature string, requester fosite.Requester) error {
	events.Trace(ctx, events.RefreshTokenIssued, events.WithRequest(requester))
	return otelx.WithSpan(ctx, "persistence.sql.CreateRefreshTokenSession", func(ctx context.Context) error {
		return p.createSession(ctx, signature, requester, sqlTableRefresh)
	})
}

func (p *Persister) GetRefreshTokenSession(ctx context.Context, signature string, session fosite.Session) (request fosite.Requester, err error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.GetRefreshTokenSession")
	defer otelx.End(span, &err)
	return p.findSessionBySignature(ctx, signature, session, sqlTableRefresh)
}

func (p *Persister) DeleteRefreshTokenSession(ctx context.Context, signature string) error {
	return otelx.WithSpan(ctx, "persistence.sql.DeleteRefreshTokenSession", func(ctx context.Context) error {
		return p.deleteSessionBySignature(ctx, signature, sqlTableRefresh)
	})
}

func (p *Persister) CreateOpenIDConnectSession(ctx context.Context, signature string, requester fosite.Requester) error {
	events.Trace(ctx, events.IdentityTokenIssued, events.WithRequest(requester))
	return otelx.WithSpan(ctx, "persistence.sql.CreateOpenIDConnectSession", func(ctx context.Context) error {
		return p.createSession(ctx, signature, requester, sqlTableOpenID)
	})
}

func (p *Persister) GetOpenIDConnectSession(ctx context.Context, signature string, requester fosite.Requester) (_ fosite.Requester, err error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.GetOpenIDConnectSession")
	defer otelx.End(span, &err)
	return p.findSessionBySignature(ctx, signature, requester.GetSession(), sqlTableOpenID)
}

func (p *Persister) DeleteOpenIDConnectSession(ctx context.Context, signature string) error {
	return otelx.WithSpan(ctx, "persistence.sql.DeleteOpenIDConnectSession", func(ctx context.Context) error {
		return p.deleteSessionBySignature(ctx, signature, sqlTableOpenID)
	})
}

func (p *Persister) GetPKCERequestSession(ctx context.Context, signature string, session fosite.Session) (_ fosite.Requester, err error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.GetPKCERequestSession")
	defer otelx.End(span, &err)
	return p.findSessionBySignature(ctx, signature, session, sqlTablePKCE)
}

func (p *Persister) CreatePKCERequestSession(ctx context.Context, signature string, requester fosite.Requester) error {
	return otelx.WithSpan(ctx, "persistence.sql.CreatePKCERequestSession", func(ctx context.Context) error {
		return p.createSession(ctx, signature, requester, sqlTablePKCE)
	})
}

func (p *Persister) DeletePKCERequestSession(ctx context.Context, signature string) error {
	return otelx.WithSpan(ctx, "persistence.sql.DeletePKCERequestSession", func(ctx context.Context) error {
		return p.deleteSessionBySignature(ctx, signature, sqlTablePKCE)
	})
}

func (p *Persister) RevokeRefreshToken(ctx context.Context, id string) error {
	return otelx.WithSpan(ctx, "persistence.sql.RevokeRefreshToken", func(ctx context.Context) error {
		return p.deactivateSessionByRequestID(ctx, id, sqlTableRefresh)
	})
}

func (p *Persister) RevokeRefreshTokenMaybeGracePeriod(ctx context.Context, id string, _ string) error {
	return otelx.WithSpan(ctx, "persistence.sql.RevokeRefreshTokenMaybeGracePeriod", func(ctx context.Context) error {
		return p.deactivateSessionByRequestID(ctx, id, sqlTableRefresh)
	})
}

func (p *Persister) RevokeAccessToken(ctx context.Context, id string) error {
	return otelx.WithSpan(ctx, "persistence.sql.RevokeAccessToken", func(ctx context.Context) error {
		return p.deleteSessionByRequestID(ctx, id, sqlTableAccess)
	})
}

func (p *Persister) flushInactiveTokens(ctx context.Context, notAfter time.Time, limit int, batchSize int, table tableName, lifespan time.Duration) (err error) {
	/* #nosec G201 table is static */
	// The value of notAfter should be the minimum between input parameter and token max expire based on its configured age
	requestMaxExpire := time.Now().Add(-lifespan)
	if requestMaxExpire.Before(notAfter) {
		notAfter = requestMaxExpire
	}

	totalDeletedCount := 0
	for deletedRecords := batchSize; totalDeletedCount < limit && deletedRecords == batchSize; {
		d := batchSize
		if limit-totalDeletedCount < batchSize {
			d = limit - totalDeletedCount
		}
		// Delete in batches
		// The outer SELECT is necessary because our version of MySQL doesn't yet support 'LIMIT & IN/ALL/ANY/SOME subquery
		deletedRecords, err = p.Connection(ctx).RawQuery(
			fmt.Sprintf(`DELETE FROM %s WHERE signature in (
				SELECT signature FROM (SELECT signature FROM %s hoa WHERE requested_at < ? and nid = ? ORDER BY requested_at LIMIT %d ) as s
			)`, OAuth2RequestSQL{Table: table}.TableName(), OAuth2RequestSQL{Table: table}.TableName(), d),
			notAfter,
			p.NetworkID(ctx),
		).ExecWithCount()
		totalDeletedCount += deletedRecords

		if err != nil {
			break
		}
		p.l.Debugf("Flushing tokens...: %d/%d", totalDeletedCount, limit)
	}
	p.l.Debugf("Flush Refresh Tokens flushed_records: %d", totalDeletedCount)
	return sqlcon.HandleError(err)
}

func (p *Persister) FlushInactiveAccessTokens(ctx context.Context, notAfter time.Time, limit int, batchSize int) error {
	return otelx.WithSpan(ctx, "persistence.sql.FlushInactiveAccessTokens", func(ctx context.Context) error {
		return p.flushInactiveTokens(ctx, notAfter, limit, batchSize, sqlTableAccess, p.config.GetAccessTokenLifespan(ctx))
	})
}

func (p *Persister) FlushInactiveRefreshTokens(ctx context.Context, notAfter time.Time, limit int, batchSize int) error {
	return otelx.WithSpan(ctx, "persistence.sql.FlushInactiveRefreshTokens", func(ctx context.Context) error {
		return p.flushInactiveTokens(ctx, notAfter, limit, batchSize, sqlTableRefresh, p.config.GetRefreshTokenLifespan(ctx))
	})
}

func (p *Persister) DeleteAccessTokens(ctx context.Context, clientID string) error {
	return otelx.WithSpan(ctx, "persistence.sql.DeleteAccessTokens", func(ctx context.Context) error {
		/* #nosec G201 table is static */
		return sqlcon.HandleError(
			p.QueryWithNetwork(ctx).Where("client_id=?", clientID).Delete(&OAuth2RequestSQL{Table: sqlTableAccess}),
		)
	})
}
