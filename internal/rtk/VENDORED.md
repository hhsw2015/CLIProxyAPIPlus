# Vendored RTK (Rust Token Killer) Go port

Source: https://github.com/Icehunter/conduit (commit pinned at vendor time, MIT)
Original: https://github.com/rtk-ai/rtk (Apache 2.0)

This is a Go in-process port of RTK's tool-output compression filters.
Used to compress verbose `tool_result` blocks (git diff, grep, ls, tree, build
output, etc.) before forwarding requests upstream, saving 30-90% tokens on
common dev operations.

Both upstreams are active. To resync:

    cd /tmp && git clone --depth=1 https://github.com/Icehunter/conduit
    rm -f internal/rtk/*.go internal/rtk/track/*.go
    cp /tmp/conduit/internal/rtk/*.go internal/rtk/
    cp /tmp/conduit/internal/rtk/track/*.go internal/rtk/track/
    go build ./... && go test ./internal/rtk/...

If conduit lags behind upstream RTK, port specific filters from
https://github.com/rtk-ai/rtk/tree/master/src/cmds (Rust → Go).

License: Apache 2.0 (LICENSE-APACHE in this directory).
