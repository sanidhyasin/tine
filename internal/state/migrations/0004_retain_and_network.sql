-- Snapshots kept until they're deleted, and VMs without a network.

-- A retained snapshot is kept until someone deletes it: it isn't garbage
-- once its VM is destroyed, and it doesn't count against the VM's limit of
-- snapshots (it has a limit of its own). Only a VM's snapshot, of the kind
-- 'vm', can be retained.
ALTER TABLE snapshots ADD COLUMN retained INTEGER NOT NULL DEFAULT 0
    CHECK (retained IN (0, 1));

-- The VM's network: 'default' (a namespace with NAT to the internet) or
-- 'none' (loopback only: a tap device connected to nothing). NULL for VMs
-- from before this migration, which all had the default.
ALTER TABLE vms ADD COLUMN network TEXT
    CHECK (network IS NULL OR network IN ('default', 'none'));
