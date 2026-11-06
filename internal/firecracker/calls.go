package firecracker

import (
	"context"
	"errors"
	"net/http"
	"net/url"
)

// Configuration calls. Except for the logger and metrics, these only work
// before StartInstance.

// PutLogger configures Firecracker's own log.
func (c *Client) PutLogger(ctx context.Context, l Logger) error {
	return c.do(ctx, http.MethodPut, "/logger", "/logger", l, nil)
}

// PutMetrics configures where Firecracker writes its metrics.
func (c *Client) PutMetrics(ctx context.Context, m Metrics) error {
	return c.do(ctx, http.MethodPut, "/metrics", "/metrics", m, nil)
}

// PutMachineConfig sets the VM's vCPUs and memory.
func (c *Client) PutMachineConfig(ctx context.Context, m MachineConfig) error {
	return c.do(ctx, http.MethodPut, "/machine-config", "/machine-config", m, nil)
}

// GetMachineConfig returns the VM's current machine configuration.
func (c *Client) GetMachineConfig(ctx context.Context) (MachineConfig, error) {
	var m MachineConfig
	err := c.do(ctx, http.MethodGet, "/machine-config", "/machine-config", nil, &m)
	return m, err
}

// PutBootSource sets the kernel and its command line.
func (c *Client) PutBootSource(ctx context.Context, b BootSource) error {
	return c.do(ctx, http.MethodPut, "/boot-source", "/boot-source", b, nil)
}

// PutDrive adds a drive, or replaces the one with the same DriveID.
func (c *Client) PutDrive(ctx context.Context, d Drive) error {
	if d.DriveID == "" {
		return errors.New("firecracker PUT /drives/{drive_id}: DriveID is empty")
	}
	return c.do(ctx, http.MethodPut, "/drives/{drive_id}", "/drives/"+url.PathEscape(d.DriveID), d, nil)
}

// PutNetworkInterface adds a network interface, or replaces the one with the same IfaceID.
func (c *Client) PutNetworkInterface(ctx context.Context, n NetworkInterface) error {
	if n.IfaceID == "" {
		return errors.New("firecracker PUT /network-interfaces/{iface_id}: IfaceID is empty")
	}
	return c.do(ctx, http.MethodPut, "/network-interfaces/{iface_id}", "/network-interfaces/"+url.PathEscape(n.IfaceID), n, nil)
}

// PutVsock adds the vsock device.
func (c *Client) PutVsock(ctx context.Context, v Vsock) error {
	return c.do(ctx, http.MethodPut, "/vsock", "/vsock", v, nil)
}

// PutEntropy adds a virtio-rng device.
func (c *Client) PutEntropy(ctx context.Context, e EntropyDevice) error {
	return c.do(ctx, http.MethodPut, "/entropy", "/entropy", e, nil)
}

// Actions.

// StartInstance boots the configured VM.
func (c *Client) StartInstance(ctx context.Context) error {
	return c.action(ctx, "InstanceStart")
}

// SendCtrlAltDel asks the guest to shut down, as pressing Ctrl+Alt+Del would (x86 only).
// With the usual "reboot=k" kernel argument, the Firecracker process exits once the guest is down.
func (c *Client) SendCtrlAltDel(ctx context.Context) error {
	return c.action(ctx, "SendCtrlAltDel")
}

// FlushMetrics writes the current metrics to the metrics file now.
func (c *Client) FlushMetrics(ctx context.Context) error {
	return c.action(ctx, "FlushMetrics")
}

func (c *Client) action(ctx context.Context, actionType string) error {
	return c.do(ctx, http.MethodPut, "/actions", "/actions", instanceAction{ActionType: actionType}, nil)
}

// Running state.

// Pause freezes the VM's vCPUs. The VM's memory and devices stay as they are.
func (c *Client) Pause(ctx context.Context) error {
	return c.do(ctx, http.MethodPatch, "/vm", "/vm", vmState{State: "Paused"}, nil)
}

// Resume unfreezes a paused VM. Resuming a running VM does nothing.
func (c *Client) Resume(ctx context.Context) error {
	return c.do(ctx, http.MethodPatch, "/vm", "/vm", vmState{State: "Resumed"}, nil)
}

// Information.

// DescribeInstance returns the VM's state and the Firecracker version running it.
func (c *Client) DescribeInstance(ctx context.Context) (InstanceInfo, error) {
	var info InstanceInfo
	err := c.do(ctx, http.MethodGet, "/", "/", nil, &info)
	return info, err
}

// Version returns the Firecracker version, e.g. "1.17.0".
func (c *Client) Version(ctx context.Context) (string, error) {
	var v firecrackerVersion
	err := c.do(ctx, http.MethodGet, "/version", "/version", nil, &v)
	return v.FirecrackerVersion, err
}
