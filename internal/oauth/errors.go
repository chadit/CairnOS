package oauth

import (
	"errors"
	"fmt"

	"github.com/chadit/CairnOS/internal/tenancy"
)

// Sentinel errors for the OAuth handler. The browser-facing responses stay
// generic; these let tests and logs name the real cause.
var (
	// ErrStateInvalid reports a callback whose state value is unknown,
	// expired, or already used. A mismatched state is the CSRF signal, so the
	// callback refuses it.
	ErrStateInvalid = errors.New("oauth state invalid or expired")

	// ErrNoIDToken reports an exchanged token carrying no id_token, so there is
	// no identity to verify.
	ErrNoIDToken = errors.New("oauth response carried no id_token")

	// ErrNoSubject reports an ID token whose verified claims carry no subject,
	// leaving nothing to key a membership on.
	ErrNoSubject = errors.New("id token carried no subject")

	// ErrRoleUnknown reports a stored or configured role outside the defined
	// role set, which would grant an empty scope set.
	ErrRoleUnknown = errors.New("workspace role is not a known role")

	// ErrAutoProvisionDisabled reports a first-time login refused because
	// auto-provisioning is off (spec H1). An admin must pre-create the
	// membership before the subject can log in.
	ErrAutoProvisionDisabled = errors.New("oauth auto-provisioning is disabled")

	// ErrSubjectNotAllowed reports a first-time login refused because the
	// subject's email domain or subject id is outside the auto-provision
	// allowlist (spec H1).
	ErrSubjectNotAllowed = errors.New("oauth subject is not allowed to auto-provision")
)

// errUnknownStoredRole wraps ErrRoleUnknown with the offending role name.
func errUnknownStoredRole(role string) error {
	return fmt.Errorf("%w: %q", ErrRoleUnknown, role)
}

// errResolveMember wraps a tenancy GetMemberRole failure that is not the
// expected not-a-member case.
func errResolveMember(err error) error {
	return fmt.Errorf("resolve member role: %w", err)
}

// errProvisionMember wraps a tenancy AddMember failure during first login.
func errProvisionMember(err error) error {
	return fmt.Errorf("provision member: %w", err)
}

// errAutoProvisionDisabled wraps ErrAutoProvisionDisabled with the subject that
// was refused, for the server-side log.
func errAutoProvisionDisabled(subject string) error {
	return fmt.Errorf("%w: subject %q has no membership and auto-provisioning is off", ErrAutoProvisionDisabled, subject)
}

// errSubjectNotAllowed wraps ErrSubjectNotAllowed with the refused subject.
func errSubjectNotAllowed(subject string) error {
	return fmt.Errorf("%w: subject %q", ErrSubjectNotAllowed, subject)
}

// isMemberNotFound reports whether err is tenancy's not-a-member signal, the
// case the handler treats as a first login to auto-provision.
func isMemberNotFound(err error) bool {
	return errors.Is(err, tenancy.ErrMemberNotFound)
}
