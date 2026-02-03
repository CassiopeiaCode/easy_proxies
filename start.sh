#!/bin/bash
# Ensure config.yaml is writable for WebUI settings, but keep nodes.txt read-only.
touch config.yaml nodes.txt 2>/dev/null
chmod 666 config.yaml 2>/dev/null || true
chmod 444 nodes.txt 2>/dev/null || true
docker compose pull && docker compose down && docker compose up -d
