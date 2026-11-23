# Oberth agent client

The agent client runs on macOS and Linux and connects to an Oberth server on Linux. The run box
uses the fetcher's install and the joined checkout's uplink profile unless its options name the
absolute executable path and uplink profile printed by `oberth join`; keep the executable outside
a checkout. The box shows up to `runs` runs, three by default or when blank, none at 0. The person
has `/oberth-runs` and `/oberth-watch`; the model uses the `oberth mcp` bridge, and the plugin
registers no model tools (they would share the bridge's server name).
For several upstreams, set **Uplink profile** (`profile`) to the comma-separated joined
profile names, for example `work,personal`. The box and `/oberth-runs` merge them newest
first with one total row limit; blank uses the environment or joined checkout selection.

The run box requires a host with function-hook modules and the `atom`, `read`
and `update` state API, with plugin validation and test harness support.

The standalone fetcher needs curl or wget, the `openssl` command, and sha256sum
or shasum. The curl, `openssl` (LibreSSL) and shasum supplied by macOS meet these prerequisites. The
fetcher verifies the signed release before installing it into the user bin
directory; fetching does not execute the downloaded client.
