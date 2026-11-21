# Oberth agent client

The agent client runs on macOS and Linux and connects to an Oberth server on
Linux. Configure the run box with the absolute verified executable path and
uplink profile printed by `oberth join`. Keep the executable outside a checkout.

The run box requires a host with function-hook modules and the `atom`, `read`
and `update` state API, with plugin validation and test harness support.

The standalone fetcher needs curl or wget, the `openssl` command, and sha256sum
or shasum. The curl, `openssl` (LibreSSL) and shasum supplied by macOS meet these prerequisites. The
fetcher verifies the signed release before installing it into the user bin
directory; fetching does not execute the downloaded client.
