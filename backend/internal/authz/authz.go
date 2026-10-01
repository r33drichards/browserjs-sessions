// Package authz answers "may this user do this to this session?".
package authz

import "context"

type Permission string

const (
	View   Permission = "can_view"   // see the session, open its VNC view
	Manage Permission = "can_manage" // rename, stop, resume, delete, call its MCP endpoint
)

// Authorizer is implemented by Topaz in production and by Memory in tests.
// A Check on a session the Authorizer does not know is false for everyone.
type Authorizer interface {
	Check(ctx context.Context, userID, sessionID string, p Permission) (bool, error)
	// AddSession records sessionID as owned by ownerID.
	AddSession(ctx context.Context, sessionID, ownerID string) error
	RemoveSession(ctx context.Context, sessionID string) error
	// SetAdmin adds or removes userID from the admins group.
	SetAdmin(ctx context.Context, userID string, admin bool) error
}
