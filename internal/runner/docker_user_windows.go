//go:build windows

package runner

// containerUser leaves the image's own user in place.
//
// Windows has no uid to hand a Linux container, and Docker Desktop's bind
// mounts do not carry host ownership through in the first place — files land
// with whatever the sharing layer decides regardless of who wrote them. Naming
// a numeric user here would refuse to start for no benefit.
func containerUser() string { return "" }
