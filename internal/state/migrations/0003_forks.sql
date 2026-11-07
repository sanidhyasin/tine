-- Forks: one snapshot of a running VM, restored many times at once.

-- Snapshots gain a kind, 'fork': taken to fork a VM, and garbage as soon as
-- no VM runs from it. SQLite can't change a CHECK constraint in place, so
-- the table is rebuilt. No other table refers to it.
CREATE TABLE snapshots_new (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL CHECK (kind IN ('vm', 'template', 'fork')),
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
INSERT INTO snapshots_new (id, kind, state, vm_id, image, vcpus, memory_mib, size_bytes, metadata, created_at, updated_at)
    SELECT id, kind, state, vm_id, image, vcpus, memory_mib, size_bytes, metadata, created_at, updated_at FROM snapshots;
DROP TABLE snapshots;
ALTER TABLE snapshots_new RENAME TO snapshots;
CREATE INDEX snapshots_vm ON snapshots (vm_id);
CREATE INDEX snapshots_templates ON snapshots (image, vcpus, memory_mib, created_at) WHERE kind = 'template';

-- The VM a VM was forked from. Its snapshot_id is the fork's snapshot.
ALTER TABLE vms ADD COLUMN parent_id TEXT;
CREATE INDEX vms_parent ON vms (parent_id) WHERE parent_id IS NOT NULL;
