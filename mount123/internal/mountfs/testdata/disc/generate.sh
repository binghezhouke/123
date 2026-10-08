#!/bin/sh
set -eu

# Rebuilds the checked-in ISO fixtures. Requires genisoimage 1.1.11 or compatible.
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
TOOL=${GENISOIMAGE:-genisoimage}
mkdir -p "$HERE/images"

# Normalize source metadata so the filesystem metadata in the images is stable.
find "$HERE/payload" -type d -exec chmod 755 {} +
find "$HERE/payload" -type f -exec chmod 644 {} +
find "$HERE/payload" -depth -exec touch -d @1577934245 {} +

build() {
  out=$1
  volume=$2
  shift 2
  "$TOOL" -quiet -iso-level 3 -creation-date 1577934245 \
    -input-charset UTF-8 -output-charset UTF-8 \
    -volid "$volume" -appid MOUNT123-TEST -publisher MOUNT123 \
    -uid 0 -gid 0 -dir-mode 0555 -file-mode 0444 \
    -o "$HERE/images/$out" "$@"
}

build iso9660-base.iso M123BASE "$HERE/payload/base"
build joliet-unicode.iso M123JOLIET -J -joliet-long "$HERE/payload/joliet"
build rockridge-long-path.iso M123RR -R "$HERE/payload/rockridge"
build udf-bridge.iso M123BRIDGE -R -J -joliet-long -udf "$HERE/payload/bridge"
build large-listing.iso M123LARGE -R "$HERE/payload/listing"
