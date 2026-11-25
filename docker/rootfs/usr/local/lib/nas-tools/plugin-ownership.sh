# Both s6 services pass the bundled Go server path before starting a plugin manager.
unset NASTOOL_DISK_SPACE_OWNER NASTOOL_MTEAM_RSS_OWNER
if [[ ${NASTOOL_UI_MODE:-legacy} == react && -x $1 ]]; then
    local_backend="http://127.0.0.1:${NASTOOL_LEGACY_PORT:-3002}"
    if [[ ${NASTOOL_LEGACY_URL:-$local_backend} == "$local_backend" ]]; then
        export NASTOOL_DISK_SPACE_OWNER=go NASTOOL_MTEAM_RSS_OWNER=go
    fi
    unset local_backend
fi
