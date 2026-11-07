// Package e2e holds tine's end-to-end tests. They drive the real tined
// service on the dev server through its API, as a user would, and check
// that nothing is left behind on the host afterwards.
//
// They're tagged "e2e" and need root, KVM, a running tined and the "base"
// image. Run them from your Mac with:
//
//	make e2e
package e2e
