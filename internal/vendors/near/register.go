package near

import "github.com/abhijeet-oxide/softwareGateway/internal/vendors"

// Register adds this layout to a registry.
//
// An explicit call rather than an init side effect. A blank import that
// silently changes how packages are grouped is the kind of action-at-a-distance
// that makes a bug take a day to find; the composition root names its vendors
// out loud, in one place, where `grep near` finds them.
func Register(r *vendors.Registry) { r.Register(Layout{}) }
