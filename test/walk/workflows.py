"""Author native workflow fixtures; JSON is also valid YAML."""
from copy import deepcopy

SOURCE_STEPS = 6


def source():
    return {"artifacts": [{"name": "source", "path": "/work/src", "mode": 493,
                           "plugin": {"name": "oberth", "key": "{{workflow.parameters.oberth-source}}",
                                      "configuration": '{"version":1,"kind":"main"}'}}]}


def container(image, command, env=None):
    return {"image": image, "workingDir": "/work/src", "command": command,
            "env": [{"name": k, "value": v} for k, v in (env or {}).items()]}


def workflow(templates, entrypoint):
    return {"apiVersion": "argoproj.io/v1alpha1", "kind": "Workflow",
            "spec": {"entrypoint": entrypoint, "activeDeadlineSeconds": 600, "templates": templates}}


def fixtures(image, marker, registry, image_name):
    env = {"EXPECTED_COMMIT": "{{workflow.parameters.oberth-sha}}", "EXPECTED_MARKER": marker}
    build = {"name": "build", "inputs": source(), "container": container(image, ["sh", "./check-source.sh"], env)}
    readonly = {"name": "readonly", "inputs": source(),
                "container": container(image, ["python3", "./check-readonly.py"])}
    failing = {"name": "failing", "inputs": source(),
               "container": container(image, ["sh", "./check-failure.sh"], env)}
    restart = {"name": "source-check", "inputs": source(),
               "container": container(image, ["sh", "./check-restart.sh"], env)}
    restart_root = {"name": "restart", "steps": [[{"name": "source-" + str(i), "template": "source-check"}]
                                                 for i in range(1, SOURCE_STEPS + 1)]}
    restart_workflow = workflow([restart_root, restart], "restart")
    restart_workflow["spec"]["suspend"] = True
    # A distinct readonly run lets a source-mount failure remain an independent
    # observation instead of concealing the build, logs and release checks.
    release = {"name": "publish", "inputs": source(),
               "volumes": [{"name": "private-files", "emptyDir": {"medium": "Memory", "sizeLimit": "16Mi"}}],
               "container": container(image, ["oberth", "secretstore", "exec", "--dir=/run/private-files/owned",
                                                "--path=oberth/data/release/walk", "--", "sh", "/work/src/release.sh"],
                                      {**env, "COSIGN_PASSWORD": "",
                                       "RELEASE_TAG": "{{workflow.parameters.oberth-release-tag}}",
                                       "WALK_REGISTRY": registry, "WALK_IMAGE": image_name})}
    release["container"]["volumeMounts"] = [
        {"name": "private-files", "mountPath": "/run/private-files"}]
    denied = {"name": "needs-secret", "container": container(image, ["oberth", "secretstore", "exec",
              "--path=oberth/data/release/walk", "--", "true"])}
    invalid = deepcopy(build)
    invalid["name"] = "offender-two"
    invalid["walkUnknownTwo"] = True
    first = deepcopy(build)
    first["name"] = "offender-one"
    first["walkUnknownOne"] = True
    invalid_root = {"name": "invalid", "steps": [[{"name": "one", "template": "offender-one"},
                                                  {"name": "two", "template": "offender-two"}]]}
    return {"build": workflow([build], "build"), "release": workflow([release], "publish"),
            "readonly": workflow([readonly], "readonly"), "ungranted": workflow([denied], "needs-secret"),
            "invalid": workflow([invalid_root, first, invalid], "invalid"), "failing": workflow([failing], "failing"),
            "restart": restart_workflow}
