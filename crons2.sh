#!/bin/sh

# Read the URL from the environment variable. If it is not set, use the default value
URL="${XMONITOR_DBS2_URL:-http://okx-defi-xlayer-xmonitor-pro:7001/api/v1/dbs2}"

# Read the URL from the environment variable. If it is not set, use the default value
WORK_DIR="${XMONITOR_S2FILE_DIR:-/datax/erigon-data/chaindata}"

echo "[$(date)] Configuration information: URL=$URL, WORK_DIR=$WORK_DIR"

cd "$WORK_DIR" || exit


MACHINE_ID="$(hostname)_$(head -c 32 /dev/urandom | md5sum | cut -c1-8)"

while true; do
  NOW=$(date +%H:%M)
  if [ "$NOW" == "11:58" ]; then

      RAW_SIZE=$(du -sh mdb.dat | awk '{print $1}')


      PARAM1=$(echo "$RAW_SIZE" | awk '
        /K$/ {val=substr($0,1,length($0)-1); printf "%.2f", val/1024/1024}
        /M$/ {val=substr($0,1,length($0)-1); printf "%.2f", val/1024}
        /G$/ {val=substr($0,1,length($0)-1); printf "%.2f", val}
        /T$/ {val=substr($0,1,length($0)-1); printf "%.2f", val*1024}
      ')

      echo "[$(date)] Sending request with db_sizes2=${PARAM1}GB, machine_id=${MACHINE_ID}"

      curl -s -X POST "$URL" \
        -H "Content-Type: application/json" \
        -d "{\"db_sizes2\": $PARAM1, \"machine_id\": \"$MACHINE_ID\"}"

      echo -e "\n---"


    sleep 61
  else
    sleep 30
  fi
done