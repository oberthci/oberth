# Go module access

Oberth no longer provides a Go module proxy or injects module resolver settings
or proxy CA files into Workflows. Repositories configure their own module
sources and access to private modules in their Workflow environment.

The upgrade command automatically removes stored `argo.goProxy` settings and
prints one upgrade line. Remove these settings from maintained chart values
files before applying them again. Direct chart upgrades must also clear the
retired settings. The proxy listener, serve flags and chart resources are gone.

This page remains as the target of links in historical release notes. See
[standalone deployment configuration](standalone-configuration.md) for current
upgrade configuration.
