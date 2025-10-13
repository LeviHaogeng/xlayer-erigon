#!/bin/sh

# Read the URL from the environment variable. If it is not set, use the default value
URL="${XMONITOR_DB_URL:-http://okx-defi-xlayer-xmonitor-pro:7001/api/v1/db}"

# Read the URL from the environment variable. If it is not set, use the default value
FILES="${XMONITOR_FILES:-/data/erigon-data/chaindata/mdbx.dat /data/erigon-data/smt/mdbx.dat}"
#FILES="/Users/oker/meili/chunsheng.wang_dacs_at_okg.com/117/Downloads/DACSDMK/xmonitor/AZB1.zip /Users/oker/meili/chunsheng.wang_dacs_at_okg.com/117/Downloads/DACSDMK/xmonitor/AZB.zip"

echo "[$(date)] Configuration information: URL=$URL, FILES=$FILES"

convert_to_gb() {
  RAW_SIZE=$1
  echo "$RAW_SIZE" | awk '
    /K$/ {val=substr($0,1,length($0)-1); printf "%.2f", val/1024/1024}
    /M$/ {val=substr($0,1,length($0)-1); printf "%.2f", val/1024}
    /G$/ {val=substr($0,1,length($0)-1); printf "%.2f", val}
    /T$/ {val=substr($0,1,length($0)-1); printf "%.2f", val*1024}
  '
}

while true; do
  NOW=$(date +%H:%M)
  if [ "$NOW" == "11:59" ]; then
      # Initialize parameters
      PARAM1="0"
      PARAM2="0"
      INDEX=1

      for FILE in $FILES; do
          if [ -f "$FILE" ]; then
              RAW_SIZE=$(du -sh "$FILE" | awk '{print $1}')
              PARAM=$(convert_to_gb "$RAW_SIZE")
              echo "[$(date)] $FILE -> ${PARAM}GB"


              if [ $INDEX -eq 1 ]; then
                  PARAM1=$PARAM
              elif [ $INDEX -eq 2 ]; then
                  PARAM2=$PARAM
              fi
          else
              echo "[$(date)] WARNING: $FILE NOT HAVE"
          fi
          INDEX=$((INDEX+1))
      done


      JSON="{\"chaindata\": $PARAM1, \"smt\": $PARAM2}"
      echo "Sending request: $JSON"

      curl -s -X POST "$URL" \
        -H "Content-Type: application/json" \
        -d "$JSON"
      echo ""
      echo "---"
      sleep 61
  else
    sleep 30
  fi
done