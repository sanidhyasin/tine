#!/usr/bin/env bash
# Manage tine's AWS dev server. Run this on your Mac, from the tine folder.
#
#   ./scripts/dev-server.sh up           create the server (or start it), with its data volume
#   ./scripts/dev-server.sh setup        sync this repo, then run host-setup.sh on the server
#   ./scripts/dev-server.sh status       show its state, IP, settings and data volume
#   ./scripts/dev-server.sh ssh [cmd]    SSH in (or run one command)
#   ./scripts/dev-server.sh sync         copy this repo to ~/tine on the server
#   ./scripts/dev-server.sh stop         stop it (no compute charges while stopped)
#   ./scripts/dev-server.sh allow-my-ip  let your current IP in over SSH (after it changes)
#   ./scripts/dev-server.sh down         terminate it for good (deletes both of its disks)
#
# Cost in ap-south-1 (on-demand, checked 2026-11-07): m8i.xlarge is $0.2227/hour
# while running, plus $0.005/hour for the public IPv4 address while running.
# The disks cost $0.0912 per GB-month even while stopped: about $5.47/month for
# the 60 GB root disk and $4.56/month for the 50 GB data volume.
#
# The server stops itself after 60 minutes with no SSH connections and no
# running Firecracker VMs.
#
# The AWS profile is TINE_AWS_PROFILE: there's no default.

set -euo pipefail

PROFILE="${TINE_AWS_PROFILE:?set TINE_AWS_PROFILE to the AWS profile to use}"
REGION="$(aws configure get region --profile "$PROFILE" 2>/dev/null || true)"
REGION="${REGION:-ap-south-1}"
TYPE="${TINE_INSTANCE_TYPE:-m8i.xlarge}"   # 4 vCPU, 16 GiB. c8i/m8i/r8i support nested virtualization.
DISK_GB="${TINE_DISK_GB:-60}"
DATA_GB="${TINE_DATA_GB:-50}"              # mounted at /srv/tine as XFS by host-setup.sh
DATA_DEVICE_NAME="/dev/sdf"                # EC2's name for it; Linux sees an NVMe disk
GP3_PRICE_GB_MONTH="0.0912"                # ap-south-1, checked 2026-11-07
NAME="tine-dev"
SSH_KEY="${TINE_SSH_KEY:-$HOME/.ssh/id_ed25519}"
KNOWN_HOSTS="$HOME/.ssh/known_hosts_tine"
AMI_PARAM="/aws/service/canonical/ubuntu/server/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id"
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

SSH_OPTS=(-i "$SSH_KEY" -o "UserKnownHostsFile=$KNOWN_HOSTS" -o StrictHostKeyChecking=accept-new -o ConnectTimeout=5)

aws_() { aws --profile "$PROFILE" --region "$REGION" "$@"; }
say()  { printf '==> %s\n' "$*" >&2; }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

# macOS ships bash 3.2, which switches off `set -e` inside $(...). Functions
# whose output gets captured therefore turn it back on themselves, and every
# captured value is checked with `expect`.
# Usage: expect <glob> <value> <error message>   (one glob only: "a|b" doesn't work here)
expect() {
  # shellcheck disable=SC2254
  case "$2" in $1) ;; *) die "$3" ;; esac
}

instance_id() {
  set -e
  local id
  id="$(aws_ ec2 describe-instances \
    --filters "Name=tag:Name,Values=$NAME" "Name=tag:Project,Values=tine" \
              "Name=instance-state-name,Values=pending,running,stopping,stopped" \
    --query 'Reservations[].Instances[].InstanceId | [0]' --output text)"
  # "None" means there's no server yet; anything else must be an instance ID.
  [ "$id" = "None" ] || expect 'i-*' "$id" "Couldn't query EC2 with the $PROFILE profile"
  echo "$id"
}

require_instance() {
  set -e
  local id
  id="$(instance_id)"
  [ "$id" != "None" ] || die "There's no $NAME server. Run: $0 up"
  echo "$id"
}

instance_field() {
  set -e
  aws_ ec2 describe-instances --instance-ids "$1" --query "Reservations[0].Instances[0].$2" --output text
}

running_ip() {
  set -e
  local id ip
  id="$(require_instance)"
  ip="$(instance_field "$id" PublicIpAddress)"
  [ "$ip" != "None" ] || die "$NAME isn't running. Run: $0 up"
  expect '[0-9]*.[0-9]*.[0-9]*.[0-9]*' "$ip" "Couldn't read the public IP of $id"
  echo "$ip"
}

my_ip() {
  set -e
  local ip
  ip="$(curl -fsS https://checkip.amazonaws.com)"
  ip="$(printf '%s' "$ip" | tr -d '[:space:]')"
  expect '[0-9]*.[0-9]*.[0-9]*.[0-9]*' "$ip" "Couldn't find your public IPv4 address"
  echo "$ip"
}

security_group() {
  set -e
  local vpc sg
  vpc="$(aws_ ec2 describe-vpcs --filters Name=isDefault,Values=true --query 'Vpcs[0].VpcId' --output text)"
  expect 'vpc-*' "$vpc" "No default VPC in $REGION"
  sg="$(aws_ ec2 describe-security-groups \
        --filters "Name=group-name,Values=$NAME" "Name=vpc-id,Values=$vpc" \
        --query 'SecurityGroups[0].GroupId' --output text)"
  if [ "$sg" = "None" ]; then
    # AWS only allows a-zA-Z0-9 and . _-:/()#,@[]+=&;{}!$* in descriptions (no apostrophes).
    sg="$(aws_ ec2 create-security-group --group-name "$NAME" --vpc-id "$vpc" \
          --description "tine dev server - SSH from the owner IP only" \
          --tag-specifications "ResourceType=security-group,Tags=[{Key=Project,Value=tine}]" \
          --query GroupId --output text)"
    expect 'sg-*' "$sg" "Couldn't create the $NAME security group"
    say "Created security group $sg"
  fi
  expect 'sg-*' "$sg" "Couldn't find the $NAME security group"
  echo "$sg"
}

allow_my_ip() {
  local sg ip perms
  sg="$(security_group)"
  expect 'sg-*' "$sg" "No security group to update"
  ip="$(my_ip)"
  expect '[0-9]*.[0-9]*.[0-9]*.[0-9]*' "$ip" "Couldn't find your public IPv4 address"
  perms="$(aws_ ec2 describe-security-groups --group-ids "$sg" \
           --query 'SecurityGroups[0].IpPermissions' --output json)"
  if [ "$(printf '%s' "$perms" | tr -d '[:space:]')" != "[]" ]; then
    aws_ ec2 revoke-security-group-ingress --group-id "$sg" --ip-permissions "$perms" >/dev/null
  fi
  aws_ ec2 authorize-security-group-ingress --group-id "$sg" \
    --protocol tcp --port 22 --cidr "$ip/32" >/dev/null
  say "SSH allowed from $ip only"
}

key_pair() {
  [ -f "$SSH_KEY.pub" ] || die "No SSH public key at $SSH_KEY.pub (set TINE_SSH_KEY to use another key)"
  if ! aws_ ec2 describe-key-pairs --key-names "$NAME" >/dev/null 2>&1; then
    aws_ ec2 import-key-pair --key-name "$NAME" --public-key-material "fileb://$SSH_KEY.pub" \
      --tag-specifications "ResourceType=key-pair,Tags=[{Key=Project,Value=tine}]" >/dev/null
    say "Imported $SSH_KEY.pub as key pair $NAME"
  fi
}

default_subnet() {
  # Pick an availability zone that offers the instance type, then its default subnet.
  set -e
  local zones az subnet
  zones="$(aws_ ec2 describe-instance-type-offerings --location-type availability-zone \
           --filters "Name=instance-type,Values=$TYPE" \
           --query 'InstanceTypeOfferings[].Location' --output text)"
  az="$(printf '%s' "$zones" | tr '\t' '\n' | sort | head -1)"
  expect "$REGION*" "$az" "$TYPE isn't offered in $REGION"
  subnet="$(aws_ ec2 describe-subnets \
            --filters Name=default-for-az,Values=true "Name=availability-zone,Values=$az" \
            --query 'Subnets[0].SubnetId' --output text)"
  expect 'subnet-*' "$subnet" "No default subnet in $az"
  echo "$subnet"
}

data_volume_id() {
  # The data volume attached to instance $1, or "None".
  set -e
  local vol
  vol="$(aws_ ec2 describe-volumes \
         --filters "Name=attachment.instance-id,Values=$1" "Name=tag:Project,Values=tine" "Name=tag:Role,Values=data" \
         --query 'Volumes[0].VolumeId' --output text)"
  [ "$vol" = "None" ] || expect 'vol-*' "$vol" "Couldn't look up the data volume of $1"
  echo "$vol"
}

ensure_data_volume() {
  # Make sure instance $1 has its data volume attached, creating it if needed.
  local id="$1" vol az state cost
  vol="$(data_volume_id "$id")"
  [ "$vol" = "None" ] || return 0

  az="$(instance_field "$id" Placement.AvailabilityZone)"
  expect "$REGION*" "$az" "Couldn't read the availability zone of $id"

  # Reuse a detached data volume from an earlier attempt instead of paying for a second one.
  vol="$(aws_ ec2 describe-volumes \
         --filters "Name=tag:Project,Values=tine" "Name=tag:Role,Values=data" \
                   "Name=status,Values=available" "Name=availability-zone,Values=$az" \
         --query 'Volumes[0].VolumeId' --output text)"
  if [ "$vol" = "None" ]; then
    cost="$(awk -v g="$DATA_GB" -v p="$GP3_PRICE_GB_MONTH" 'BEGIN { printf "%.2f", g * p }')"
    say "Creating a $DATA_GB GB gp3 data volume in $az (about \$$cost/month)"
    vol="$(aws_ ec2 create-volume --availability-zone "$az" --size "$DATA_GB" --volume-type gp3 \
           --tag-specifications "ResourceType=volume,Tags=[{Key=Name,Value=$NAME-data},{Key=Project,Value=tine},{Key=Role,Value=data}]" \
           --query VolumeId --output text)"
    expect 'vol-*' "$vol" "Couldn't create the data volume"
    aws_ ec2 wait volume-available --volume-ids "$vol"
  else
    expect 'vol-*' "$vol" "Couldn't look up detached data volumes"
    say "Reusing the detached data volume $vol"
  fi

  say "Attaching $vol to $id as $DATA_DEVICE_NAME"
  aws_ ec2 attach-volume --volume-id "$vol" --instance-id "$id" --device "$DATA_DEVICE_NAME" >/dev/null
  for _ in $(seq 1 30); do
    state="$(aws_ ec2 describe-volumes --volume-ids "$vol" --query 'Volumes[0].Attachments[0].State' --output text)"
    [ "$state" = "attached" ] && break
    sleep 2
  done
  [ "$state" = "attached" ] || die "$vol didn't attach (state: $state)"

  # Delete it together with the server, so `down` never leaves a paid volume behind.
  aws_ ec2 modify-instance-attribute --instance-id "$id" \
    --block-device-mappings "[{\"DeviceName\":\"$DATA_DEVICE_NAME\",\"Ebs\":{\"DeleteOnTermination\":true}}]"
  say "Data volume $vol attached. Run '$0 setup' to format and mount it."
}

user_data() {
  cat <<'USERDATA'
#!/bin/bash
# Installed by tine's scripts/dev-server.sh.
# Stops this dev server after 60 idle minutes (no SSH connections and no
# Firecracker VMs). Turn it off with: sudo systemctl disable --now tine-idle-stop.timer
cat > /usr/local/bin/tine-idle-stop <<'EOF'
#!/bin/bash
IDLE_MINUTES=60
STAMP=/run/tine-idle-since
if [ -n "$(ss -Htn state established '( sport = :22 )')" ] || pgrep -x firecracker >/dev/null; then
  rm -f "$STAMP"
  exit 0
fi
[ -f "$STAMP" ] || date +%s > "$STAMP"
if [ $(( ($(date +%s) - $(cat "$STAMP")) / 60 )) -ge "$IDLE_MINUTES" ]; then
  logger "tine-idle-stop: idle for ${IDLE_MINUTES} minutes, powering off"
  systemctl poweroff
fi
EOF
chmod 755 /usr/local/bin/tine-idle-stop

cat > /etc/systemd/system/tine-idle-stop.service <<'EOF'
[Unit]
Description=Stop the tine dev server when it's idle

[Service]
Type=oneshot
ExecStart=/usr/local/bin/tine-idle-stop
EOF

cat > /etc/systemd/system/tine-idle-stop.timer <<'EOF'
[Unit]
Description=Check every 5 minutes whether the tine dev server is idle

[Timer]
OnBootSec=10min
OnUnitActiveSec=5min

[Install]
WantedBy=timers.target
EOF

systemctl daemon-reload
systemctl enable --now tine-idle-stop.timer
USERDATA
}

wait_for_ssh() {
  local ip="$1"
  say "Waiting for SSH on $ip"
  for _ in $(seq 1 40); do
    if ssh "${SSH_OPTS[@]}" -o BatchMode=yes "ubuntu@$ip" true 2>/dev/null; then
      return 0
    fi
    sleep 5
  done
  die "SSH didn't come up on $ip. Try: $0 allow-my-ip, then $0 ssh"
}

cmd_up() {
  local id state ip sg subnet ud
  id="$(instance_id)"
  if [ "$id" != "None" ]; then
    state="$(instance_field "$id" State.Name)"
    case "$state" in
      stopped)  allow_my_ip; say "Starting $id"; aws_ ec2 start-instances --instance-ids "$id" >/dev/null ;;
      stopping) die "$id is still stopping. Try again in a minute." ;;
      *)        allow_my_ip; say "$id is already $state" ;;
    esac
  else
    key_pair
    sg="$(security_group)"
    expect 'sg-*' "$sg" "No security group to launch with"
    allow_my_ip
    subnet="$(default_subnet)"
    expect 'subnet-*' "$subnet" "No subnet to launch in"
    ud="$(mktemp)"
    user_data > "$ud"
    say "Launching $TYPE with nested virtualization in $REGION (AWS profile: $PROFILE)"
    id="$(aws_ ec2 run-instances \
          --image-id "resolve:ssm:$AMI_PARAM" \
          --instance-type "$TYPE" \
          --cpu-options NestedVirtualization=enabled \
          --key-name "$NAME" \
          --security-group-ids "$sg" \
          --subnet-id "$subnet" \
          --block-device-mappings "DeviceName=/dev/sda1,Ebs={VolumeSize=$DISK_GB,VolumeType=gp3,DeleteOnTermination=true}" \
          --metadata-options HttpTokens=required \
          --instance-initiated-shutdown-behavior stop \
          --user-data "file://$ud" \
          --tag-specifications \
            "ResourceType=instance,Tags=[{Key=Name,Value=$NAME},{Key=Project,Value=tine}]" \
            "ResourceType=volume,Tags=[{Key=Name,Value=$NAME},{Key=Project,Value=tine}]" \
          --query 'Instances[0].InstanceId' --output text)"
    rm -f "$ud"
    expect 'i-*' "$id" "The launch didn't return an instance ID"
    say "Launched $id"
  fi
  aws_ ec2 wait instance-running --instance-ids "$id"
  ensure_data_volume "$id"
  ip="$(instance_field "$id" PublicIpAddress)"
  expect '[0-9]*.[0-9]*.[0-9]*.[0-9]*' "$ip" "$id has no public IP address"
  wait_for_ssh "$ip"
  say "Ready. Connect with: $0 ssh   (ubuntu@$ip)"
}

cmd_setup() {
  local id ip vol dev
  id="$(require_instance)"
  ip="$(running_ip)"
  vol="$(data_volume_id "$id")"
  [ "$vol" != "None" ] || die "$NAME has no data volume yet. Run: $0 up"
  # On Nitro instances EBS volumes are NVMe disks, and udev names each one after its volume ID.
  dev="/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_${vol//-/}"
  cmd_sync
  ssh "${SSH_OPTS[@]}" "ubuntu@$ip" "cd ~/tine && TINE_DATA_DEVICE='$dev' ./scripts/host-setup.sh"
}

cmd_status() {
  local id vol
  id="$(instance_id)"
  if [ "$id" = "None" ]; then
    echo "There's no $NAME server. Run: $0 up"
    return
  fi
  aws_ ec2 describe-instances --instance-ids "$id" --output table \
    --query 'Reservations[0].Instances[0].{Id:InstanceId,State:State.Name,Type:InstanceType,IP:PublicIpAddress,Zone:Placement.AvailabilityZone,NestedVirt:CpuOptions.NestedVirtualization,Launched:LaunchTime}'
  vol="$(data_volume_id "$id")"
  if [ "$vol" = "None" ]; then
    echo "Data volume: none attached (run: $0 up)"
  else
    aws_ ec2 describe-volumes --volume-ids "$vol" --output table \
      --query 'Volumes[0].{DataVolume:VolumeId,SizeGB:Size,Type:VolumeType,State:State,DeleteWithServer:Attachments[0].DeleteOnTermination}'
  fi
}

cmd_ssh() {
  local ip
  ip="$(running_ip)"
  exec ssh "${SSH_OPTS[@]}" "ubuntu@$ip" "$@"
}

cmd_sync() {
  local ip
  ip="$(running_ip)"
  rsync -az --delete --exclude '/artifacts/' --exclude '/.git/' --exclude '.DS_Store' \
    -e "ssh ${SSH_OPTS[*]}" "$REPO_DIR/" "ubuntu@$ip:tine/"
  say "Synced $REPO_DIR to ~/tine on $ip"
}

cmd_stop() {
  local id
  id="$(require_instance)"
  aws_ ec2 stop-instances --instance-ids "$id" >/dev/null
  say "Stopping $id. Both disks are kept; run '$0 up' to start it again."
}

cmd_down() {
  local id answer
  id="$(require_instance)"
  read -r -p "This terminates $id and deletes its root disk and data volume. Type $NAME to confirm: " answer
  [ "$answer" = "$NAME" ] || die "Cancelled"
  aws_ ec2 terminate-instances --instance-ids "$id" >/dev/null
  rm -f "$KNOWN_HOSTS"
  say "Terminating $id"
}

command -v aws >/dev/null || die "The AWS CLI isn't installed"

cmd="${1:-}"
shift || true
case "$cmd" in
  up)          cmd_up ;;
  setup)       cmd_setup ;;
  status)      cmd_status ;;
  ssh)         cmd_ssh "$@" ;;
  sync)        cmd_sync ;;
  stop)        cmd_stop ;;
  allow-my-ip) allow_my_ip ;;
  down)        cmd_down ;;
  *)           sed -n '2,11p' "$0" | sed 's/^# \{0,1\}//'; exit 1 ;;
esac
