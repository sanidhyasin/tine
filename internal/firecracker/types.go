package firecracker

// Field names and enum values follow Firecracker v1.17's API spec. Optional
// fields use omitempty so Firecracker applies its own defaults; fields the
// spec marks as required never do.

// MachineConfig is the VM's size and memory setup (PUT /machine-config).
type MachineConfig struct {
	// VCPUCount must be 1 or an even number, up to 32.
	VCPUCount  int `json:"vcpu_count"`
	MemSizeMiB int `json:"mem_size_mib"`
	// SMT enables simultaneous multithreading (x86 only).
	SMT bool `json:"smt,omitempty"`
	// TrackDirtyPages turns on KVM dirty-page tracking, which diff snapshots need.
	// It isn't saved in snapshots: restores must ask for it again.
	TrackDirtyPages bool      `json:"track_dirty_pages,omitempty"`
	HugePages       HugePages `json:"huge_pages,omitempty"`
}

// HugePages selects how guest memory is backed on the host.
type HugePages string

const (
	HugePagesNone        HugePages = "None"        // regular 4 KiB pages (the default)
	HugePagesTransparent HugePages = "Transparent" // transparent huge pages via madvise
	HugePages2M          HugePages = "2M"          // explicit 2 MiB hugetlbfs pages
)

// BootSource is the kernel to boot and its command line (PUT /boot-source).
type BootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args,omitempty"`
	InitrdPath      string `json:"initrd_path,omitempty"`
}

// Drive is a virtio-block disk (PUT /drives/{drive_id}).
type Drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
	// PartUUID picks the root partition when the disk has a partition table.
	PartUUID    string       `json:"partuuid,omitempty"`
	CacheType   CacheType    `json:"cache_type,omitempty"`
	IOEngine    IOEngine     `json:"io_engine,omitempty"`
	RateLimiter *RateLimiter `json:"rate_limiter,omitempty"`
}

// CacheType is a drive's caching strategy.
type CacheType string

const (
	CacheTypeUnsafe    CacheType = "Unsafe"    // ignore guest flushes (the default)
	CacheTypeWriteback CacheType = "Writeback" // honor guest flushes
)

// IOEngine is how Firecracker does a drive's I/O.
type IOEngine string

const (
	IOEngineSync  IOEngine = "Sync"  // the default
	IOEngineAsync IOEngine = "Async" // io_uring
)

// NetworkInterface is a virtio-net device backed by a host tap device
// (PUT /network-interfaces/{iface_id}).
type NetworkInterface struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
	GuestMAC    string `json:"guest_mac,omitempty"`
	// MTU, if set, is advertised to the guest.
	MTU           int          `json:"mtu,omitempty"`
	RxRateLimiter *RateLimiter `json:"rx_rate_limiter,omitempty"`
	TxRateLimiter *RateLimiter `json:"tx_rate_limiter,omitempty"`
}

// Vsock is the host-guest socket device (PUT /vsock). The host connects to
// UDSPath and sends "CONNECT <port>\n" to reach a guest port.
type Vsock struct {
	// GuestCID is the guest's vsock address. It must be at least 3.
	GuestCID uint32 `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

// EntropyDevice is a virtio-rng device that gives the guest host randomness
// (PUT /entropy).
type EntropyDevice struct {
	RateLimiter *RateLimiter `json:"rate_limiter,omitempty"`
}

// RateLimiter limits a device's bandwidth and operations.
type RateLimiter struct {
	Bandwidth *TokenBucket `json:"bandwidth,omitempty"`
	Ops       *TokenBucket `json:"ops,omitempty"`
}

// TokenBucket refills Size tokens every RefillTimeMs milliseconds.
type TokenBucket struct {
	Size         int64  `json:"size"`
	OneTimeBurst *int64 `json:"one_time_burst,omitempty"`
	RefillTimeMs int64  `json:"refill_time"`
}

// Logger configures Firecracker's own log (PUT /logger).
type Logger struct {
	LogPath       string   `json:"log_path,omitempty"`
	Level         LogLevel `json:"level,omitempty"`
	ShowLevel     bool     `json:"show_level,omitempty"`
	ShowLogOrigin bool     `json:"show_log_origin,omitempty"`
	Module        string   `json:"module,omitempty"`
}

// LogLevel is a Firecracker log level.
type LogLevel string

const (
	LogLevelError   LogLevel = "Error"
	LogLevelWarning LogLevel = "Warning"
	LogLevelInfo    LogLevel = "Info"
	LogLevelDebug   LogLevel = "Debug"
	LogLevelTrace   LogLevel = "Trace"
	LogLevelOff     LogLevel = "Off"
)

// Metrics configures where Firecracker writes its JSON metrics (PUT /metrics).
type Metrics struct {
	MetricsPath string `json:"metrics_path"`
	// EmitID adds the jailer's --id to every metrics line.
	EmitID     bool              `json:"emit_id,omitempty"`
	Properties map[string]string `json:"properties,omitempty"`
}

// InstanceInfo describes the Firecracker process and its VM (GET /).
type InstanceInfo struct {
	AppName    string        `json:"app_name"`
	ID         string        `json:"id"`
	State      InstanceState `json:"state"`
	VMMVersion string        `json:"vmm_version"`
}

// InstanceState is the VM's state as Firecracker reports it.
type InstanceState string

const (
	StateNotStarted InstanceState = "Not started"
	StateRunning    InstanceState = "Running"
	StatePaused     InstanceState = "Paused"
)

// Request bodies that callers don't build themselves.
type (
	instanceAction struct {
		ActionType string `json:"action_type"`
	}
	vmState struct {
		State string `json:"state"`
	}
	firecrackerVersion struct {
		FirecrackerVersion string `json:"firecracker_version"`
	}
)
