// auth.go authenticates and authorizes S3 requests. Authentication verifies
// the SigV4 signature against the access key's secret; authorization then
// evaluates the owning user's grants with pkg/auth — the same resolver the
// storage core uses — so the S3 surface enforces exactly the §7.2 model
// (§14).
package s3

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/hyperkubeorg/databox/pkg/auth"
)

// systemRecord is the shape the `.databox/` system endpoints return.
type systemRecord struct {
	Key   string `json:"key"`
	Value []byte `json:"value"`
	Rev   uint64 `json:"rev"`
}

// caller is the authenticated principal behind an S3 request.
type caller struct {
	user   string
	grants []auth.Grant
	isRoot bool
	// key is the API key that authenticated this request; its scope (if
	// any) caps every operation regardless of the user's grants.
	key auth.AccessKey
}

// authenticate verifies the request's SigV4 signature — header auth
// (Authorization: AWS4-HMAC-SHA256 ...) or presigned query auth (X-Amz-*
// parameters) — and resolves the calling user plus their grants. It
// returns an error suitable for an S3 error response on any failure.
func (g *gateway) authenticate(ctx context.Context, r *http.Request) (*caller, error) {
	now := time.Now()
	var keyID string
	// verify runs the transport-appropriate signature check once the
	// access key's secret is known.
	var verify func(secret string) bool

	switch {
	case r.Header.Get("Authorization") != "":
		info, err := parseAuthHeader(r.Header.Get("Authorization"))
		if err != nil {
			return nil, err
		}
		// Bound the replay window: the signed timestamp must be within
		// the configured skew of the gateway clock (default ±15 min).
		if err := checkClockSkew(r.Header.Get("x-amz-date"), now, g.opts.ClockSkew); err != nil {
			return nil, err
		}
		keyID = info.keyID
		verify = func(secret string) bool { return verifySignature(r, info, secret) }

	case isPresigned(r.URL.Query()):
		p, err := parsePresigned(r.URL.Query())
		if err != nil {
			return nil, err
		}
		// Presigned parity is GET/PUT only (§14); HEAD rides along since
		// SDKs issue it with GET-shaped URLs.
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodPut:
		default:
			return nil, fmt.Errorf("presigned %s not supported", r.Method)
		}
		// The URL is valid from its mint time until mint+expires (plus
		// skew tolerance on both edges).
		if err := checkPresignedWindow(p, now, g.opts.ClockSkew); err != nil {
			return nil, err
		}
		keyID = p.keyID
		verify = func(secret string) bool { return verifyPresigned(r, p, secret) }

	default:
		return nil, fmt.Errorf("missing authentication (Authorization header or presigned parameters)")
	}

	// Resolve the access key (secret + owning user) from the system view.
	ak, err := g.lookupAccessKey(ctx, keyID)
	if err != nil {
		return nil, fmt.Errorf("unknown access key")
	}
	if !verify(ak.Secret) {
		return nil, fmt.Errorf("signature mismatch")
	}
	// Load the user's grants (root bypasses all checks, §7.1).
	c := &caller{user: ak.User, isRoot: ak.User == auth.RootUser, key: ak}
	if !c.isRoot {
		u, err := g.lookupUser(ctx, ak.User)
		if err != nil {
			return nil, fmt.Errorf("cannot load user %q: %v", ak.User, err)
		}
		c.grants = u.Grants
	}
	return c, nil
}

// authorize checks a (key, verb) against the API key's scope AND the
// owning user's grants (§7.2). Scope binds first and binds everyone —
// including root's keys: a scoped credential is narrow no matter who
// owns it, which is the whole point of minting one.
func (c *caller) authorize(key string, verb auth.Verb) bool {
	if !c.key.InScope(key) {
		return false
	}
	if c.isRoot {
		return true
	}
	return auth.Allowed(c.grants, key, verb)
}

// lookupAccessKey reads accesskeys/<keyID> from the system keyspace.
func (g *gateway) lookupAccessKey(ctx context.Context, keyID string) (auth.AccessKey, error) {
	var rec systemRecord
	if err := g.admin.Raw(ctx, http.MethodGet, "/api/v1/system/accesskeys/"+keyID, nil, &rec); err != nil {
		return auth.AccessKey{}, err
	}
	return auth.DecodeAccessKey(rec.Value)
}

// lookupUser reads users/<name> from the system keyspace for grant checks.
func (g *gateway) lookupUser(ctx context.Context, name string) (auth.User, error) {
	var rec systemRecord
	if err := g.admin.Raw(ctx, http.MethodGet, "/api/v1/system/users/"+name, nil, &rec); err != nil {
		return auth.User{}, err
	}
	var u auth.User
	if err := json.Unmarshal(rec.Value, &u); err != nil {
		return auth.User{}, err
	}
	return u, nil
}
