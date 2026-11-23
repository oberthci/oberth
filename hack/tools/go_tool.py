"""Select the module closure for trusted Go tool builds."""
import os


def args(root, action, *arguments):
    modfile = os.environ.get("OBERTH_TOOLS_MODFILE", str(root / "go.mod"))
    return ["go", action, "-modfile=" + modfile, *arguments]


def environment():
    return {**os.environ, "GOWORK": "off", "GOFLAGS": "-mod=readonly -p=2 -buildvcs=false"}
