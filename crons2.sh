#!/bin/sh

URL="${XMONITOR_DBS2_URL:-http://okx-defi-xlayer-xmonitor-pro:7001/api/v1/dbs2}"

MDB_FILE="${XMONITOR_S2FILE_DIR:-/data/erigon-data/chaindata/mdbx.dat}"

echo "[$(date)] config informations: URL=$URL, MDB_FILE=$MDB_FILE"

if [ ! -f "$MDB_FILE" ]; then
    echo "[$(date)] err: file not have: $MDB_FILE"
    exit 1
fi

MACHINE_ID="$(hostname)_$(head -c 32 /dev/urandom | md5sum | cut -c1-8)"

while true; do
  NOW=$(date +%H:%M)
  if [ "$NOW" == "11:58" ]; then

      RAW_SIZE=$(du -sh "$MDB_FILE" | awk '{print $1}')


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