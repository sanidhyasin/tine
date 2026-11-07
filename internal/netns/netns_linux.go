//go:build linux

package netns

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
	vnetns "github.com/vishvananda/netns"
)

// nftTimeout bounds one nft run.
const nftTimeout = 10 * time.Second

// Setup prepares the host for VM networks: it turns on IPv4 forwarding and
// installs the host's firewall and NAT rules. tined runs it at startup. It's
// safe to repeat.
func Setup(ctx context.Context) error {
	if err := writeSysctl("net/ipv4/ip_forward", "1"); err != nil {
		return fmt.Errorf("network setup: enable IPv4 forwarding: %w", err)
	}
	if err := applyNft(ctx, HostRuleset()); err != nil {
		return fmt.Errorf("network setup: install host firewall: %w", err)
	}
	return nil
}

// RemoveHostRules deletes the host's "inet tine" table. Only tests and
// uninstalls need it: running VMs depend on these rules.
func RemoveHostRules(ctx context.Context) error {
	return applyNft(ctx, "add table inet tine\ndelete table inet tine\n")
}

// Create builds n: its namespace, the veth pair, the tap device owned by
// owner, addresses, routes, forwarding and firewall rules. If any step
// fails, it removes everything it made.
func Create(ctx context.Context, n Network, owner Owner) (err error) {
	if owner.UID <= 0 || owner.GID <= 0 {
		return fmt.Errorf("create network %s: tap owner must not be root, got %d:%d", n.ID, owner.UID, owner.GID)
	}
	if err := newNamedNamespace(n.Namespace); err != nil {
		return fmt.Errorf("create network %s: namespace: %w", n.ID, err)
	}
	defer func() {
		if err != nil {
			_ = Destroy(n)
		}
	}()

	ns, err := vnetns.GetFromName(n.Namespace)
	if err != nil {
		return fmt.Errorf("create network %s: open namespace: %w", n.ID, err)
	}
	defer func() { _ = ns.Close() }()

	// The veth's peer is created straight inside the namespace, so its fixed
	// name never clashes with another VM's.
	veth := &netlink.Veth{
		LinkAttrs:     netlink.LinkAttrs{Name: n.HostVeth},
		PeerName:      NamespaceVeth,
		PeerNamespace: netlink.NsFd(int(ns)),
	}
	if err := netlink.LinkAdd(veth); err != nil {
		return fmt.Errorf("create network %s: veth %s: %w", n.ID, n.HostVeth, err)
	}
	if err := configureLink(n.HostVeth, n.HostCIDR()); err != nil {
		return fmt.Errorf("create network %s: host veth: %w", n.ID, err)
	}

	err = inNamespace(ns, func() error {
		if err := configureLink("lo", netip.Prefix{}); err != nil {
			return fmt.Errorf("loopback: %w", err)
		}
		if err := configureLink(NamespaceVeth, n.NamespaceCIDR()); err != nil {
			return fmt.Errorf("namespace veth: %w", err)
		}
		veth0, err := netlink.LinkByName(NamespaceVeth)
		if err != nil {
			return err
		}
		if err := netlink.RouteAdd(&netlink.Route{LinkIndex: veth0.Attrs().Index, Gw: n.HostIP.AsSlice()}); err != nil {
			return fmt.Errorf("default route: %w", err)
		}
		// A tap device is created in the namespace of the thread that opens
		// /dev/net/tun, which is why this runs here. It persists after this
		// call, and only its owner may attach to it: the VM's Firecracker.
		tap := &netlink.Tuntap{
			LinkAttrs: netlink.LinkAttrs{Name: TapDevice},
			Mode:      netlink.TUNTAP_MODE_TAP,
			Flags:     netlink.TUNTAP_NO_PI | netlink.TUNTAP_VNET_HDR,
			Owner:     uint32(owner.UID), //nolint:gosec // checked to be positive above
			Group:     uint32(owner.GID), //nolint:gosec // as above
		}
		if err := netlink.LinkAdd(tap); err != nil {
			return fmt.Errorf("tap %s: %w", TapDevice, err)
		}
		if err := configureLink(TapDevice, netip.PrefixFrom(GatewayIP, 30)); err != nil {
			return fmt.Errorf("tap %s: %w", TapDevice, err)
		}
		if err := writeSysctl("net/ipv4/ip_forward", "1"); err != nil {
			return fmt.Errorf("enable forwarding: %w", err)
		}
		if err := applyNft(ctx, NamespaceRuleset()); err != nil {
			return fmt.Errorf("firewall: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("create network %s: in namespace %s: %w", n.ID, n.Namespace, err)
	}
	return nil
}

// Destroy removes n's veth pair and namespace. Stop the VM's Firecracker
// first: until it exits, the kernel keeps the namespace and its tap device
// alive. Destroying a network that's already gone is fine.
func Destroy(n Network) error {
	var errs []error
	// Deleting the host's end of a veth pair deletes both ends.
	if link, err := netlink.LinkByName(n.HostVeth); err == nil {
		if err := netlink.LinkDel(link); err != nil {
			errs = append(errs, fmt.Errorf("delete veth %s: %w", n.HostVeth, err))
		}
	} else if !isLinkNotFound(err) {
		errs = append(errs, fmt.Errorf("find veth %s: %w", n.HostVeth, err))
	}
	if err := removeNamedNamespace(n.Namespace); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("destroy network %s: %w", n.ID, err)
	}
	return nil
}

// Leftovers lists the VM networks on this host: namespaces named tine-*
// and veths named tine<N>. Once every VM is destroyed, it should be empty.
func Leftovers() ([]string, error) {
	ids, veths, err := List()
	if err != nil {
		return nil, err
	}
	var found []string
	for _, id := range ids {
		found = append(found, "namespace "+NamespacePrefix+id)
	}
	for _, v := range veths {
		found = append(found, "veth "+v)
	}
	return found, nil
}

// List returns what VM networking exists on this host: the IDs of VMs with
// a network namespace, and the names of host veths (tine<N>).
func List() (vmIDs, veths []string, err error) {
	entries, err := os.ReadDir(NamespaceDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, err
	}
	for _, e := range entries {
		if id, ok := strings.CutPrefix(e.Name(), NamespacePrefix); ok && validID.MatchString(id) {
			vmIDs = append(vmIDs, id)
		}
	}
	links, err := netlink.LinkList()
	if err != nil {
		return nil, nil, err
	}
	for _, l := range links {
		if name := l.Attrs().Name; l.Type() == "veth" && strings.HasPrefix(name, HostVethPrefix) {
			veths = append(veths, name)
		}
	}
	return vmIDs, veths, nil
}

// DestroyNamespace removes a VM's network namespace knowing only the VM's
// ID, for leftovers whose slot is unknown. Removing the namespace also
// removes the veth pair and tap device inside it, once nothing uses them.
func DestroyNamespace(vmID string) error {
	if !validID.MatchString(vmID) {
		return fmt.Errorf("destroy namespace: bad VM ID %q", vmID)
	}
	return removeNamedNamespace(NamespacePrefix + vmID)
}

// DeleteVeth removes a host veth (and so its peer). A missing one is fine.
func DeleteVeth(name string) error {
	if !strings.HasPrefix(name, HostVethPrefix) {
		return fmt.Errorf("delete veth %q: not one of tine's", name)
	}
	link, err := netlink.LinkByName(name)
	if isLinkNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return netlink.LinkDel(link)
}

// configureLink adds addr to the named link (unless addr is the zero
// Prefix) and brings it up.
func configureLink(name string, addr netip.Prefix) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return err
	}
	if addr.IsValid() {
		ipnet := &net.IPNet{IP: addr.Addr().AsSlice(), Mask: net.CIDRMask(addr.Bits(), addr.Addr().BitLen())}
		if err := netlink.AddrAdd(link, &netlink.Addr{IPNet: ipnet}); err != nil {
			return fmt.Errorf("add %s to %s: %w", addr, name, err)
		}
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bring up %s: %w", name, err)
	}
	return nil
}

func isLinkNotFound(err error) bool {
	var notFound netlink.LinkNotFoundError
	return errors.As(err, &notFound)
}

// inNamespace runs fn on an OS thread that has joined ns. Network calls in fn
// (netlink, /proc/sys/net, processes it starts) then act inside ns.
//
// fn runs on its own goroutine, so the caller's thread never changes
// namespace. If that thread can't be moved back afterwards, it stays locked,
// and the Go runtime throws it away instead of reusing it in the wrong
// namespace.
func inNamespace(ns vnetns.NsHandle, fn func() error) error {
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		orig, err := vnetns.Get()
		if err != nil {
			errc <- fmt.Errorf("get current namespace: %w", err)
			return
		}
		defer func() { _ = orig.Close() }()
		if err := vnetns.Set(ns); err != nil {
			errc <- fmt.Errorf("join namespace: %w", err)
			return
		}
		fnErr := fn()
		if err := vnetns.Set(orig); err != nil {
			errc <- errors.Join(fnErr, fmt.Errorf("return to the host namespace: %w", err))
			return
		}
		runtime.UnlockOSThread()
		errc <- fnErr
	}()
	return <-errc
}

// newNamedNamespace creates a network namespace and pins it at
// NamespaceDir/<name>, like `ip netns add`. It fails if the name is taken.
func newNamedNamespace(name string) error {
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		orig, err := vnetns.Get()
		if err != nil {
			errc <- fmt.Errorf("get current namespace: %w", err)
			return
		}
		defer func() { _ = orig.Close() }()
		// NewNamed moves this thread into the new namespace.
		ns, err := vnetns.NewNamed(name)
		if err == nil {
			_ = ns.Close()
		}
		if setErr := vnetns.Set(orig); setErr != nil {
			errc <- errors.Join(err, fmt.Errorf("return to the host namespace: %w", setErr))
			return
		}
		runtime.UnlockOSThread()
		errc <- err
	}()
	return <-errc
}

// removeNamedNamespace unpins a namespace made by newNamedNamespace. The
// kernel frees it once nothing else uses it. A missing one is fine.
func removeNamedNamespace(name string) error {
	path := NamespaceDir + "/" + name
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := vnetns.DeleteNamed(name); err != nil {
		// A half-made namespace can leave the file without the mount on it.
		if errors.Is(err, syscall.EINVAL) {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("remove namespace %s: %w", name, err)
			}
			return nil
		}
		return fmt.Errorf("remove namespace %s: %w", name, err)
	}
	return nil
}

// writeSysctl sets a kernel setting under /proc/sys. Settings under net/
// apply to the network namespace of the calling thread.
func writeSysctl(key, value string) error {
	return os.WriteFile("/proc/sys/"+key, []byte(value), 0o644) //nolint:gosec // key is one of tine's fixed settings
}

// applyNft runs an nftables script in one atomic transaction: all of it
// applies, or none of it does. It runs in the calling thread's network namespace.
func applyNft(ctx context.Context, script string) error {
	ctx, cancel := context.WithTimeout(ctx, nftTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nft: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
