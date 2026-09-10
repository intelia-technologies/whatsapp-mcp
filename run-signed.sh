#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
BINARY="$SCRIPT_DIR/whatsapp-mcp"
IDENTITY="${WHATSAPP_MCP_CODESIGN_IDENTITY:-Intelia WhatsApp MCP Local Signing}"
IDENTIFIER="com.intelia.whatsapp-mcp"

if [[ ! -x "$BINARY" ]]; then
    echo "ERROR: WhatsApp MCP binary not found or not executable: $BINARY" >&2
    exit 1
fi

# Go produces an ad-hoc signature whose cdhash changes on every build. Signing
# with this trusted local identity gives TCC a stable designated requirement,
# so Downloads access survives future rebuilds.
/usr/bin/codesign \
    --force \
    --sign "$IDENTITY" \
    --identifier "$IDENTIFIER" \
    --timestamp=none \
    "$BINARY"
/usr/bin/codesign --verify --strict "$BINARY"

exec "$BINARY"
