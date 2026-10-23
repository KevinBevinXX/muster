package connectors

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
)

// A VIRTUAL CONNECTOR is one whose credential is not a stored secret but is
// produced per request by something muster holds — today the GitHub App
// (internal/githubapp), which mints a short-lived installation token limited to
// the calling agent's own repository.
//
// 🔴 IT IS PRESENTED THROUGH THE SAME [Store] AS EVERY OTHER CONNECTOR, AND THAT
// IS THE POINT. Every reader that asks "does connector X exist" — the agent's
// GET /agent/connectors/{name} (which ccd's git routing and clone retry read),
// the operator's request card (Approve vs "Enter credential…"), the approve
// route's existence check, the Connectors tab list — goes through
// GetConnector/ListConnectors. Wrapping the store ([WithVirtual]) answers all of
// them in one place, so a request filed BEFORE the virtual connector existed
// becomes approvable the moment it does, with nothing re-filed and no row edited.
// The grant flow is untouched: a grant names its connector BY NAME
// (connector_grants.connector_name, not a foreign key), and the owner's
// approval is shared by their agents on the SAME repository only (a standing
// grant keyed on SourceApp and the repository: StandingRepo, migration 0043).
//
// 🔴 IT EXISTS ONLY FOR THE PRINCIPALS allow ADMITS. The App is the
// deployment's, so on a multi-user muster it is the admin's alone (cmd/
// muster-server passes authz.IsAdmin): every reader runs under the principal of
// the user it acts for — the agent's OWNER on the agent routes and the proxy,
// the approver on the approve route, who row-level security makes the grant's
// owner — and a member sees exactly the store beneath, as if no App were
// configured. A member's agent therefore never gets
// an App token, and its github request is never approvable through the App.
//
// 🔴 A VIRTUAL NAME SHADOWS A STORED ROW OF THE SAME NAME, for an admitted
// principal: its stored `github` (a personal access token) is not listed or
// used, and cannot be created, rotated or deleted while the App is configured.
// Its row is not touched. Grants record which of the two they were approved
// for (Grant.Source), so switching between them re-opens the approval
// ([ReconcileGrant]) rather than silently changing what an approval grants.

// Virtual is a connector whose credential is minted per request.
type Virtual interface {
	// Describe is the connector's public description. Its Name is the name it
	// answers to; its Hosts are the allowlist the proxy enforces BEFORE asking for
	// a credential.
	Describe() Connector
	// Credential returns the secret to inject for caller's request to host/rest
	// (rest as the proxy received it: escaped, no leading slash). host is already
	// on Describe().Hosts. anonymous=true means forward the request with NO
	// credential at all (secret is then ""). A request the connector refuses for
	// this caller is [ErrTargetRefused]; any other error means no credential
	// could be produced, and its text is shown to the agent and the operator, so
	// it must never carry a secret.
	Credential(ctx context.Context, caller Caller, host, rest string) (secret string, anonymous bool, err error)
	// Rejected tells the connector the upstream answered 401 to a request that
	// carried the secret Credential returned for caller, so a cached credential
	// can be dropped.
	Rejected(caller Caller)
	// Problem is the operator-facing reason this connector cannot currently serve
	// agentID (0: any agent), or "" when nothing is known to be wrong.
	Problem(agentID int64) string
}

// ErrTargetRefused is a virtual connector's refusal of one request's target.
var ErrTargetRefused = errors.New("connectors: refused for this agent")

// ErrManaged refuses an operator write to a virtual connector's name.
var ErrManaged = fmt.Errorf("%w: this connector is provided by the server's configuration", ErrInvalid)

// WithVirtual returns s with v presented as a connector named
// v.Describe().Name to the principals allow admits (see the file comment). A
// nil v returns s unchanged; a nil allow admits nobody.
func WithVirtual(s Store, v Virtual, allow func(context.Context) bool) Store {
	if v == nil {
		return s
	}
	if allow == nil {
		allow = func(context.Context) bool { return false }
	}
	return &virtualStore{Store: s, v: v, name: v.Describe().Name, allow: allow}
}

// VirtualOf returns the virtual connector s presents under name to ctx's
// principal, if any. The proxy uses it to take the credential from the
// connector rather than from the sealed-secret table; the UI uses it to mark the
// connector as managed.
func VirtualOf(ctx context.Context, s Store, name string) (Virtual, bool) {
	vs, ok := s.(*virtualStore)
	if !ok || vs.name != name || !vs.allow(ctx) {
		return nil, false
	}
	return vs.v, true
}

// SourceFor is the credential source connector name has for ctx's principal:
// SourceApp when a virtual connector presents it, SourceStored otherwise.
func SourceFor(ctx context.Context, s Store, name string) string {
	if _, ok := VirtualOf(ctx, s, name); ok {
		return SourceApp
	}
	return SourceStored
}

// ReconcileGrant puts an approved grant back to pending when its connector's
// credential source (for ctx's principal, the grant's owner) is no longer the
// one the approval was about — the GitHub App configured over a stored token,
// or removed from under one. reopened reports that this call did it, so the
// caller can prompt the owner. Any other grant is returned unchanged.
//
// 🔴 THIS IS THE ONE PLACE THE RULE LIVES; every reader that turns a grant into
// "may this agent use the connector" calls it first (Proxy.CheckAccess, the
// agent's connector status and request routes).
func ReconcileGrant(ctx context.Context, s Store, g Grant) (Grant, bool, error) {
	if g.Status != StatusApproved {
		return g, false, nil
	}
	// 🔴 NEVER ON A FALLBACK ROLE. When the principal's role could not be read it
	// acts as member, and for an admin that makes the App connector vanish —
	// SourceFor would answer "stored" and this would re-open every App approval
	// over one database blip. The grant is left as it is; the next request with
	// a readable role decides.
	if p, ok := authz.From(ctx); ok && p.RoleUncertain {
		return g, false, nil
	}
	src := g.Source
	if src == "" {
		src = SourceStored
	}
	if src == SourceFor(ctx, s, g.Connector) {
		return g, false, nil
	}
	ng, reopened, err := s.ReopenGrant(ctx, g.ID)
	if err != nil {
		return g, false, err
	}
	return ng, reopened, nil
}

type virtualStore struct {
	Store
	v     Virtual
	name  string
	allow func(context.Context) bool
}

func (s *virtualStore) ListConnectors(ctx context.Context) ([]Connector, error) {
	list, err := s.Store.ListConnectors(ctx)
	if err != nil || !s.allow(ctx) {
		return list, err
	}
	out := make([]Connector, 0, len(list)+1)
	for _, c := range list {
		if c.Name != s.name {
			out = append(out, c)
		}
	}
	out = append(out, s.v.Describe())
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *virtualStore) GetConnector(ctx context.Context, name string) (Connector, error) {
	if name == s.name && s.allow(ctx) {
		return s.v.Describe(), nil
	}
	return s.Store.GetConnector(ctx, name)
}

func (s *virtualStore) managed(ctx context.Context, name string) error {
	if name != s.name || !s.allow(ctx) {
		return nil
	}
	return fmt.Errorf("%w (%s): it cannot be created, rotated or deleted here — unset the configuration to "+
		"manage a stored connector of that name again", ErrManaged, s.name)
}

func (s *virtualStore) CreateConnector(ctx context.Context, nc NewConnector) (Connector, error) {
	if err := s.managed(ctx, nc.Name); err != nil {
		return Connector{}, err
	}
	return s.Store.CreateConnector(ctx, nc)
}

func (s *virtualStore) RotateSecret(ctx context.Context, name, secret string) error {
	if err := s.managed(ctx, name); err != nil {
		return err
	}
	return s.Store.RotateSecret(ctx, name, secret)
}

func (s *virtualStore) DeleteConnector(ctx context.Context, name string) error {
	if err := s.managed(ctx, name); err != nil {
		return err
	}
	return s.Store.DeleteConnector(ctx, name)
}

// Credential refuses the virtual name for an admitted principal: its secret is
// per request, and only the proxy (through VirtualOf) may ask for it.
func (s *virtualStore) Credential(ctx context.Context, name string) (Connector, string, error) {
	if name == s.name && s.allow(ctx) {
		return Connector{}, "", fmt.Errorf("connectors: %s is minted per request, not stored", s.name)
	}
	return s.Store.Credential(ctx, name)
}

func (s *virtualStore) TouchConnector(ctx context.Context, name string) error {
	if name == s.name && s.allow(ctx) {
		return nil
	}
	return s.Store.TouchConnector(ctx, name)
}

// VirtualCreatedAt is a stable "created" time for a virtual connector's
// description: when this process started.
var VirtualCreatedAt = time.Now()
