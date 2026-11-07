-- tine's state: the images tined knows about, and every VM it has run.
-- Times are RFC 3339 text in UTC, so the database stays readable with the
-- sqlite3 command.

CREATE TABLE images (
    name          TEXT PRIMARY KEY,
    dir           TEXT NOT NULL,
    metadata      TEXT NOT NULL,            -- the image's image.json
    registered_at TEXT NOT NULL
) STRICT;

CREATE TABLE vms (
    id           TEXT PRIMARY KEY,
    state        TEXT NOT NULL
                 CHECK (state IN ('creating', 'running', 'destroying', 'destroyed', 'failed')),
    image        TEXT NOT NULL,
    vcpus        INTEGER NOT NULL CHECK (vcpus > 0),
    memory_mib   INTEGER NOT NULL CHECK (memory_mib > 0),
    slot         INTEGER NOT NULL CHECK (slot >= 0),   -- network slot (internal/netns)
    uid          INTEGER NOT NULL CHECK (uid > 0),     -- Firecracker's uid and gid in its jail
    pid          INTEGER,                              -- Firecracker's pid, once it's started
    error        TEXT,                                 -- why it failed
    timings      TEXT,                                 -- JSON: stage name -> milliseconds
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    expires_at   TEXT,                                 -- when the reaper destroys it
    destroyed_at TEXT
) STRICT;

-- A slot or a uid belongs to at most one VM that isn't destroyed yet.
-- (Failed VMs keep theirs until they're cleaned up.)
CREATE UNIQUE INDEX vms_live_slot ON vms (slot) WHERE state != 'destroyed';
CREATE UNIQUE INDEX vms_live_uid ON vms (uid) WHERE state != 'destroyed';
CREATE INDEX vms_state ON vms (state);
CREATE INDEX vms_expires ON vms (expires_at) WHERE expires_at IS NOT NULL;

-- Every change of a VM's state: when, from what, to what, and why.
CREATE TABLE vm_events (
    id         INTEGER PRIMARY KEY,
    vm_id      TEXT NOT NULL REFERENCES vms (id) ON DELETE CASCADE,
    at         TEXT NOT NULL,
    from_state TEXT,                                   -- NULL when the VM was created
    to_state   TEXT NOT NULL,
    detail     TEXT NOT NULL DEFAULT ''
) STRICT;
CREATE INDEX vm_events_vm ON vm_events (vm_id, id);
