// Package ownership reports whether this process still owns the homeserver.
//
// HA binds every protected effect to one authorized activation. Components
// consult an Owner before performing an effect that a successor must not
// overlap with. Standalone deployments have no Owner and are unaffected.
package ownership

// Owner reports the authorized activation. Valid must fail closed: it reports
// an error before promotion, after retirement, and whenever ownership has not
// been confirmed recently enough to be trusted.
type Owner interface {
	Generation() int64
	Identity() string
	Valid() error
}
