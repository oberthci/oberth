# Oberth agent client

The agent client runs on macOS and Linux and connects to an Oberth server on Linux. The run box
uses the fetcher's install and the joined checkout's uplink profile unless its options name the
absolute executable path and uplink profile printed by `oberth join`; keep the executable outside
a checkout. The box shows up to `runs` runs, three by default or when blank, none at 0. The person
has `/oberth-runs`; the model uses the `oberth mcp` bridge and its versioned server CI skill, and the plugin
registers no model tools (they would share the bridge's server name).
For several upstreams, set **Uplink profile** (`profile`) to the comma-separated joined
profile names, for example `work,personal`. The box and `/oberth-runs` merge them newest
first with one total row limit; blank uses the environment or joined checkout selection.
Live changes appear in about one second; idle discovery runs every 15 seconds.

The run box requires a host with function-hook modules and the `atom`, `read`
and `update` state API, with plugin validation and test harness support.

The standalone fetcher needs curl or wget, the `openssl` command, and sha256sum
or shasum. The curl, `openssl` (LibreSSL) and shasum supplied by macOS meet these prerequisites. The
fetcher verifies the signed release before installing it into the user bin
directory; fetching does not execute the downloaded client.

The bootstrap skill owns installation and joining. After discovery, approve and load the server-owned
oberth-ci skill and references. Resource reads alone do not activate it. An upgrade requires
reconnection and explicit reload, or a fresh conversation; old prose can persist in host context.
