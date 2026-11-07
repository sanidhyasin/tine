-- Snapshots: saved states of VMs, which VMs can be restored from.
-- The files (state, memory, disk, snapshot.json) live in the snapshot store,
-- internal/snapshot. This is tined's record of each one and its lifecycle.

CREATE TABLE snapshots (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL CHECK (kind IN ('vm', 'template')),
    state      TEXT NOT NULL CHECK (state IN ('creating', 'ready', 'deleting')),
    vm_id      TEXT,                                   -- the VM it was taken from
    image      TEXT NOT NULL,
    vcpus      INTEGER NOT NULL CHECK (vcpus > 0),     -- a restore always has the snapshot's size
    memory_mib INTEGER NOT NULL CHECK (memory_mib > 0),
    size_bytes INTEGER NOT NULL DEFAULT 0,             -- disk space its files take
    metadata   TEXT,                                   -- its snapshot.json, once ready
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;
CREATE INDEX snapshots_vm ON snapshots (vm_id);
CREATE INDEX snapshots_templates ON snapshots (image, vcpus, memory_mib, created_at) WHERE kind = 'template';

-- The snapshot a VM was restored from, and runs from: its memory is mapped
-- from that snapshot's memory file. A snapshot with live VMs like this is in
-- use, and can't be deleted.
ALTER TABLE vms ADD COLUMN snapshot_id TEXT;
CREATE INDEX vms_snapshot ON vms (snapshot_id) WHERE snapshot_id IS NOT NULL;

-- How the VM started: cold (booted), template (restored from its image's
-- template) or snapshot (restored from another snapshot). NULL for VMs from
-- before this migration, which were all booted.
ALTER TABLE vms ADD COLUMN source TEXT
    CHECK (source IS NULL OR source IN ('cold', 'template', 'snapshot'));
